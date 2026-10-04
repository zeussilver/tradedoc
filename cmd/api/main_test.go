package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/zeussilver/tradedoc/internal/claude"
)

// The fixtures behind MODEL_PROVIDER=fake must load with the same strict
// rules as at startup, or `make run-fake` would fail.
func TestLoadFakeFixtures(t *testing.T) {
	f, err := loadFake(filepath.Join("..", "..", "samples", "expected"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Invoice == nil || f.PackingList == nil || len(f.Candidates) == 0 {
		t.Fatalf("fixtures incomplete: invoice %v, packing list %v, %d candidates",
			f.Invoice != nil, f.PackingList != nil, len(f.Candidates))
	}
}

func TestLoadFakeRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"sample-a-invoice.json", "sample-a-packing-list.json", "sample-a-candidates.json"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "samples", "expected", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loadFake(dir); err != nil {
		t.Fatalf("copied fixtures should load: %v", err)
	}

	// Add a misspelt field to an otherwise valid file: it must be an error,
	// not a silently missing value.
	path := filepath.Join(dir, "sample-a-invoice.json")
	var inv map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &inv); err != nil {
		t.Fatal(err)
	}
	inv["invoice_numbr"] = inv["invoice_number"]
	if data, err = json.Marshal(inv); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFake(dir); err == nil || !strings.Contains(err.Error(), "invoice_numbr") {
		t.Fatalf("err = %v, want an unknown field error", err)
	}
}

func TestPositiveInt(t *testing.T) {
	tests := []struct {
		name    string
		value   string // "" = unset
		want    int64
		wantErr bool
	}{
		{name: "unset uses the fallback", value: "", want: 7},
		{name: "whole number", value: "25", want: 25},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-3", wantErr: true},
		{name: "fraction", value: "2.5", wantErr: true},
		{name: "suffix", value: "25k", wantErr: true},
		{name: "spaces", value: " 25", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_POSITIVE_INT", tt.value)
			got, err := positiveInt("TEST_POSITIVE_INT", 7)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "TEST_POSITIVE_INT") {
					t.Fatalf("err = %v, want an error naming the variable", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("positiveInt = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

// CLAUDE_MAX_INPUT_TOKENS must reach the client; unset, it stays 0 so the
// claude package applies its own default.
func TestClaudeConfig(t *testing.T) {
	tests := []struct {
		name         string
		maxInput     string
		model        string
		wantMaxInput int
		wantModel    string
		wantErr      bool
	}{
		{name: "defaults", wantMaxInput: 0, wantModel: claude.DefaultModel},
		{name: "settings passed through", maxInput: "30000", model: "claude-test-model", wantMaxInput: 30000, wantModel: "claude-test-model"},
		{name: "zero token cap is refused", maxInput: "0", wantErr: true},
		{name: "typo is refused", maxInput: "60k", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_MAX_INPUT_TOKENS", tt.maxInput)
			t.Setenv("CLAUDE_MODEL", tt.model)
			t.Setenv("CLAUDE_EFFORT", "")
			cfg, err := claudeConfig()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "CLAUDE_MAX_INPUT_TOKENS") {
					t.Fatalf("err = %v, want a CLAUDE_MAX_INPUT_TOKENS error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxInputTokens != tt.wantMaxInput || cfg.Model != tt.wantModel {
				t.Fatalf("MaxInputTokens = %d, Model = %q; want %d, %q", cfg.MaxInputTokens, cfg.Model, tt.wantMaxInput, tt.wantModel)
			}
			if cfg.APIKey != "" {
				t.Fatal("claudeConfig must not set the API key")
			}
		})
	}
}

// With both variables set, the key in the environment is used and Secrets
// Manager is not called. The empty aws.Config has no region or credentials,
// so any AWS call would fail (without touching the network).
func TestAnthropicKeyPrefersEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-local")
	t.Setenv("ANTHROPIC_SECRET_ARN", "arn:aws:secretsmanager:ap-southeast-2:123456789012:secret:tradedoc/anthropic-api-key-AbCdEf")
	key, source, err := anthropicKey(context.Background(), aws.Config{})
	if err != nil || key != "sk-test-local" || source != "env" {
		t.Fatalf("anthropicKey = %q, %q, %v; want the environment key", key, source, err)
	}
}

// A bad setting must stop startup before the key is fetched, so the error
// names the setting rather than a Secrets Manager failure.
func TestNewModelChecksSettingsBeforeFetchingKey(t *testing.T) {
	t.Setenv("MODEL_PROVIDER", "claude")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_SECRET_ARN", "arn:aws:secretsmanager:ap-southeast-2:123456789012:secret:tradedoc/anthropic-api-key-AbCdEf")
	t.Setenv("CLAUDE_MAX_INPUT_TOKENS", "lots")
	_, err := newModel(context.Background(), aws.Config{})
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_MAX_INPUT_TOKENS") {
		t.Fatalf("err = %v, want a CLAUDE_MAX_INPUT_TOKENS error", err)
	}
}
