// Package samples holds the synthetic sample documents (PDFs made by
// scripts/gen_samples.py) and the expected extractions for them. The JSON in
// expected/ is also what the offline "fake" model returns, so this test makes
// sure those fixtures still decode into, and pass, the real schema.
package samples

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/domain"
)

// decodeStrict reads expected/<name> the way cmd/api loads the fake-model
// fixtures: it rejects any field the Go type does not have, so a typo in a
// fixture ("quantiy") fails here instead of quietly becoming a missing
// value, and it rejects a value of the wrong JSON type (a quantity written
// as the number 1200 instead of the string "1200").
func decodeStrict(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("expected", name))
	if err != nil {
		t.Fatal(err)
	}
	decodeStrictBytes(t, name, data, v)
}

func decodeStrictBytes(t *testing.T, name string, data []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if dec.More() {
		t.Fatalf("%s: more than one JSON value", name)
	}
}

// checkPositions enforces the line_no rule from the extraction prompt:
// line_no is the row's 1-based position among the goods rows, so the
// invoice and packing list pair up even when they number rows differently.
// Validate() only checks that line numbers are positive and unique.
func checkPositions(t *testing.T, lineNos []int) {
	t.Helper()
	for i, n := range lineNos {
		if n != i+1 {
			t.Errorf("line_items[%d].line_no = %d, want %d (1-based position)", i, n, i+1)
		}
	}
}

func lineTotalSum(inv domain.InvoiceExtraction) int64 {
	var sum int64
	for _, l := range inv.LineItems {
		if l.LineTotalMinor.Present() {
			sum += *l.LineTotalMinor.Value
		}
	}
	return sum
}

func packageSum(pl domain.PackingListExtraction) int64 {
	var sum int64
	for _, l := range pl.LineItems {
		if l.Packages.Present() {
			sum += *l.Packages.Value
		}
	}
	return sum
}

func TestExpectedInvoices(t *testing.T) {
	tests := []struct {
		file string
		// Sample A prints a wrong total on purpose (7,302.00 instead of
		// 7,320.00); the fixture must keep the printed value, not the sum.
		wantTotalMatchesLines bool
	}{
		{"sample-a-invoice.json", false},
		{"sample-b-invoice.json", true},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			var inv domain.InvoiceExtraction
			decodeStrict(t, tc.file, &inv)
			if err := inv.Validate(); err != nil {
				t.Fatalf("schema validation failed: %v", err)
			}
			// The sample invoices print no freight, insurance or discount
			// rows. The fixture says so with an empty list rather than
			// leaving the key out, so the fake model's answer shows
			// "charges": [] like a real reading instead of null.
			switch {
			case inv.Charges == nil:
				t.Error(`charges is missing; use "charges": [] when no charge rows are printed`)
			case len(inv.Charges) != 0:
				t.Errorf("charges = %+v, want none (the sample prints no charge rows)", inv.Charges)
			}
			var lineNos []int
			for _, l := range inv.LineItems {
				lineNos = append(lineNos, l.LineNo)
			}
			checkPositions(t, lineNos)
			if !inv.TotalMinor.Present() {
				t.Fatal("total_minor is null; every sample prints a total")
			}
			got := lineTotalSum(inv) == *inv.TotalMinor.Value
			if got != tc.wantTotalMatchesLines {
				t.Errorf("lines sum to %d, total is %d: matches=%v, want %v",
					lineTotalSum(inv), *inv.TotalMinor.Value, got, tc.wantTotalMatchesLines)
			}
		})
	}
}

func TestExpectedPackingLists(t *testing.T) {
	tests := []struct {
		file string
		// Sample A prints 84 cartons although its lines add up to 82.
		wantPackagesMatchLines bool
	}{
		{"sample-a-packing-list.json", false},
		{"sample-b-packing-list.json", true},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			var pl domain.PackingListExtraction
			decodeStrict(t, tc.file, &pl)
			if err := pl.Validate(); err != nil {
				t.Fatalf("schema validation failed: %v", err)
			}
			var lineNos []int
			for _, l := range pl.LineItems {
				lineNos = append(lineNos, l.LineNo)
			}
			checkPositions(t, lineNos)
			if !pl.TotalPackages.Present() {
				t.Fatal("total_packages is null; every sample prints a total")
			}
			got := packageSum(pl) == *pl.TotalPackages.Value
			if got != tc.wantPackagesMatchLines {
				t.Errorf("lines sum to %d packages, total is %d: matches=%v, want %v",
					packageSum(pl), *pl.TotalPackages.Value, got, tc.wantPackagesMatchLines)
			}
		})
	}
}

// TestManualExtractionExample checks testdata/invoice_extraction.json, the
// request body the README posts to POST /v1/documents/{id}/extractions. It
// is decoded the way the handler decodes it (strict wrapper, then strict
// payload), so the README example can't drift away from the schema.
func TestManualExtractionExample(t *testing.T) {
	const name = "invoice_extraction.json"
	data, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var in domain.ExtractionInput
	decodeStrictBytes(t, name, data, &in)
	// The handler refuses "model:..." sources: they are reserved for
	// readings the server got from the model itself.
	if strings.HasPrefix(in.Source, "model:") {
		t.Errorf("source = %q; a manual example must not claim to come from the model", in.Source)
	}
	var inv domain.InvoiceExtraction
	decodeStrictBytes(t, name+" payload", in.Payload, &inv)
	if err := inv.Validate(); err != nil {
		t.Fatalf("schema validation failed: %v", err)
	}
	if inv.Charges == nil {
		t.Error(`charges is missing; use "charges": [] when no charge rows are printed`)
	}
}

var sixDigits = regexp.MustCompile(`^\d{6}$`)

// The candidate codes are illustrative fixtures for the fake model: candidate
// codes for a licensed customs broker to review, not answers. They are
// checked with the same rules the API applies to real model output, plus the
// rules the classification prompt gives the model: confidence is only
// medium or low (the description is the seller's unverified declaration),
// and each candidate says what a broker must still confirm.
func TestSampleACandidates(t *testing.T) {
	var cands []ai.Candidate
	decodeStrict(t, "sample-a-candidates.json", &cands)
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}

	var inv domain.InvoiceExtraction
	decodeStrict(t, "sample-a-invoice.json", &inv)
	onInvoice := map[int]bool{}
	for _, l := range inv.LineItems {
		onInvoice[l.LineNo] = true
	}

	for i, c := range cands {
		if !sixDigits.MatchString(c.HSCode) {
			t.Errorf("candidate %d: hs_code %q is not a 6-digit subheading", i, c.HSCode)
		}
		if !onInvoice[c.LineNo] {
			t.Errorf("candidate %d: line_no %d is not on the sample A invoice", i, c.LineNo)
		}
		s := domain.ClassificationSuggestion{
			LineNo: c.LineNo, Rank: c.Rank, HSCode: c.HSCode,
			Rationale: c.Rationale, Confidence: c.Confidence,
		}
		if err := s.Validate(); err != nil {
			t.Errorf("candidate %d: %v", i, err)
		}
		if c.Confidence != domain.ConfidenceMedium && c.Confidence != domain.ConfidenceLow {
			t.Errorf("candidate %d: confidence %q, want medium or low", i, c.Confidence)
		}
		if len(c.MissingInfo) == 0 {
			t.Errorf("candidate %d: missing_info should list what a broker must confirm", i)
		}
	}
}
