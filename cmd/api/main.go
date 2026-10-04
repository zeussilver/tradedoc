// Command api starts the TradeDoc Check server. The same binary runs locally
// (a normal http.Server) and on AWS Lambda (behind a Function URL); which
// store, file storage and model it uses is chosen by environment variables
// (see docs/CONTRACT.md), so there is one code path to test.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/lambdaurl"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/blob"
	"github.com/zeussilver/tradedoc/internal/claude"
	"github.com/zeussilver/tradedoc/internal/domain"
	"github.com/zeussilver/tradedoc/internal/httpapi"
	"github.com/zeussilver/tradedoc/internal/secrets"
	"github.com/zeussilver/tradedoc/internal/store"
	"github.com/zeussilver/tradedoc/internal/webui"
)

func main() {
	// JSON logs: CloudWatch (and Lambda's JSON log format) can search fields.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		// Failing at startup is deliberate: a misconfigured server should not
		// half-work. On Lambda this shows up as an init error in CloudWatch.
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

// defaultModelCallsPerDay is the global daily cap on model operations
// (extract or suggest) when MODEL_CALLS_PER_DAY is unset. Each operation
// makes up to 2 Claude requests. Terraform sets the variable explicitly on AWS.
const defaultModelCallsPerDay = 25

func run() error {
	ctx := context.Background()
	onLambda := os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" // set by the Lambda runtime

	callsPerDay, err := positiveInt("MODEL_CALLS_PER_DAY", defaultModelCallsPerDay)
	if err != nil {
		return err
	}

	// AWS credentials and region come from the standard chain: the Lambda
	// role on AWS, or your ~/.aws profile locally. Only loaded if some
	// setting needs AWS, so a fully local run needs no AWS setup at all.
	var awsCfg aws.Config
	if envOr("STORE", "memory") == "dynamodb" || envOr("BLOB", "local") == "s3" || os.Getenv("ANTHROPIC_SECRET_ARN") != "" {
		if awsCfg, err = config.LoadDefaultConfig(ctx); err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
	}

	st, err := newStore(awsCfg)
	if err != nil {
		return err
	}
	blobs, local, err := newBlobs(awsCfg)
	if err != nil {
		return err
	}
	m, err := newModel(ctx, awsCfg)
	if err != nil {
		return err
	}

	// LocalUploads must stay a nil interface on AWS (a typed nil would
	// count as "set"), so it's only assigned when local storage is used.
	var localUploads http.Handler
	if local != nil {
		localUploads = local.UploadHandler()
	}
	handler := httpapi.New(httpapi.Config{
		Store:            st,
		Blobs:            blobs,
		Extractor:        m.extractor,
		Classifier:       m.classifier,
		ModelName:        m.name,
		ModelCallsPerDay: callsPerDay,
		Static:           webui.Handler(),
		LocalUploads:     localUploads,
		UploadOrigin:     os.Getenv("UPLOAD_ORIGIN"),
	}).Handler()

	// Modes only: never keys, secret values or URLs with signatures.
	slog.Info("starting",
		"lambda", onLambda,
		"store", envOr("STORE", "memory"),
		"blob", envOr("BLOB", "local"),
		"model_provider", m.provider,
		"model", m.name,
		"key_source", m.keySource,
		"model_calls_per_day", callsPerDay,
	)

	if onLambda {
		// The Function URL uses response streaming (InvokeMode
		// RESPONSE_STREAM), which is what lambdaurl expects. Start never returns.
		lambdaurl.Start(handler)
		return nil
	}
	return serveLocal(handler)
}

func newStore(awsCfg aws.Config) (store.Store, error) {
	switch kind := envOr("STORE", "memory"); kind {
	case "memory":
		return store.NewMemory(), nil
	case "dynamodb":
		table, err := mustEnv("TABLE_NAME")
		if err != nil {
			return nil, err
		}
		// DYNAMODB_ENDPOINT points the client at DynamoDB Local for testing
		// the whole server on a laptop. Terraform never sets it on AWS.
		client := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
			if ep := os.Getenv("DYNAMODB_ENDPOINT"); ep != "" {
				o.BaseEndpoint = aws.String(ep)
			}
		})
		return store.NewDynamo(client, table), nil
	default:
		return nil, fmt.Errorf("STORE must be memory or dynamodb, got %q", kind)
	}
}

// newBlobs also returns the *blob.Local when local storage is used, because
// main has to mount its upload handler.
func newBlobs(awsCfg aws.Config) (blob.Store, *blob.Local, error) {
	switch kind := envOr("BLOB", "local"); kind {
	case "local":
		secret := []byte(os.Getenv("LOCAL_UPLOAD_SECRET"))
		if len(secret) == 0 {
			// A fresh random key each start is fine for development: it only
			// means upload URLs issued before a restart stop working.
			secret = make([]byte, 32)
			rand.Read(secret) // never fails since Go 1.24 (it crashes the program instead)
		}
		local := blob.NewLocal(envOr("DATA_DIR", "./data/uploads"), secret)
		return local, local, nil
	case "s3":
		bucket, err := mustEnv("BUCKET_NAME")
		if err != nil {
			return nil, nil, err
		}
		if os.Getenv("UPLOAD_ORIGIN") == "" {
			// Without it the page's CSP would block the browser's PUT to S3.
			return nil, nil, errors.New("UPLOAD_ORIGIN is required when BLOB=s3")
		}
		return blob.NewS3(s3.NewFromConfig(awsCfg), bucket, "uploads/"), nil, nil
	default:
		return nil, nil, fmt.Errorf("BLOB must be local or s3, got %q", kind)
	}
}

// model is the outcome of choosing a model provider. extractor and
// classifier stay nil when the model is off; the API then answers the
// model routes with 503 "model_not_configured".
type model struct {
	provider   string // claude, fake or off
	name       string // shown by GET /v1/config
	keySource  string // where the API key came from (never the key itself)
	extractor  ai.Extractor
	classifier ai.Classifier
}

func newModel(ctx context.Context, awsCfg aws.Config) (model, error) {
	keyAvailable := os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_SECRET_ARN") != ""
	provider := os.Getenv("MODEL_PROVIDER")
	if provider == "" {
		provider = "off"
		if keyAvailable {
			provider = "claude"
		}
	}

	switch provider {
	case "off":
		return model{provider: "off", keySource: "none"}, nil

	case "fake":
		f, err := loadFake(envOr("FAKE_FIXTURES_DIR", "./samples/expected"))
		if err != nil {
			return model{}, err
		}
		return model{provider: "fake", name: "fake", keySource: "none", extractor: f, classifier: f}, nil

	case "claude":
		// Settings are checked before the key is fetched, so a typo fails
		// fast without a call to Secrets Manager.
		cfg, err := claudeConfig()
		if err != nil {
			return model{}, err
		}
		key, source, err := anthropicKey(ctx, awsCfg)
		if err != nil {
			return model{}, err
		}
		cfg.APIKey = key
		c, err := claude.New(cfg)
		if err != nil {
			return model{}, fmt.Errorf("create Claude client: %w", err)
		}
		return model{provider: "claude", name: cfg.Model, keySource: source, extractor: c, classifier: c}, nil

	default:
		return model{}, fmt.Errorf("MODEL_PROVIDER must be claude, fake or off, got %q", provider)
	}
}

// claudeConfig reads the Claude settings from the environment, without the
// API key. A variable that is unset stays zero (except the model name, which
// GET /v1/config shows), and claude.New replaces zeros with its defaults, so
// each default is defined once, in the claude package.
func claudeConfig() (claude.Config, error) {
	// CLAUDE_MAX_INPUT_TOKENS caps the input of one model request (document
	// plus prompt). The client counts it with the free token-counting
	// endpoint first and refuses a bigger document as "too large" instead of
	// paying to send it. Unset: the claude package default (60 000).
	maxInput, err := positiveInt("CLAUDE_MAX_INPUT_TOKENS", 0)
	if err != nil {
		return claude.Config{}, err
	}
	return claude.Config{
		Model:          envOr("CLAUDE_MODEL", claude.DefaultModel),
		Effort:         os.Getenv("CLAUDE_EFFORT"),
		MaxInputTokens: int(maxInput),
	}, nil
}

// anthropicKey returns the API key and where it came from. Locally it's an
// environment variable; on AWS it's read from Secrets Manager once, at cold
// start, so later requests don't pay for (or depend on) that call.
//
// ANTHROPIC_API_KEY wins when both variables are set: a developer's own key
// in the shell is used and Secrets Manager is never called. Terraform sets
// only ANTHROPIC_SECRET_ARN on Lambda, because a key in Lambda's
// environment would be visible to anyone who can read the function's
// configuration.
func anthropicKey(ctx context.Context, awsCfg aws.Config) (key, source string, err error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key, "env", nil
	}
	arn := os.Getenv("ANTHROPIC_SECRET_ARN")
	if arn == "" {
		return "", "", errors.New("MODEL_PROVIDER=claude needs ANTHROPIC_API_KEY or ANTHROPIC_SECRET_ARN")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	key, err = secrets.Get(ctx, secretsmanager.NewFromConfig(awsCfg), arn)
	if err != nil {
		return "", "", err
	}
	return key, "secrets_manager", nil
}

// loadFake reads the fixture files behind MODEL_PROVIDER=fake and checks
// them with the same rules real model output must pass, so a demo can't
// show data the real pipeline would have rejected.
func loadFake(dir string) (ai.Fake, error) {
	var inv domain.InvoiceExtraction
	if err := readStrictJSON(filepath.Join(dir, "sample-a-invoice.json"), &inv); err != nil {
		return ai.Fake{}, err
	}
	if err := inv.Validate(); err != nil {
		return ai.Fake{}, fmt.Errorf("fake fixture sample-a-invoice.json: %w", err)
	}

	var pl domain.PackingListExtraction
	if err := readStrictJSON(filepath.Join(dir, "sample-a-packing-list.json"), &pl); err != nil {
		return ai.Fake{}, err
	}
	if err := pl.Validate(); err != nil {
		return ai.Fake{}, fmt.Errorf("fake fixture sample-a-packing-list.json: %w", err)
	}

	var cands []ai.Candidate
	if err := readStrictJSON(filepath.Join(dir, "sample-a-candidates.json"), &cands); err != nil {
		return ai.Fake{}, err
	}
	for i, c := range cands {
		s := domain.ClassificationSuggestion{
			LineNo: c.LineNo, Rank: c.Rank, HSCode: c.HSCode,
			Rationale: c.Rationale, Confidence: c.Confidence,
		}
		if err := s.Validate(); err != nil {
			return ai.Fake{}, fmt.Errorf("fake fixture sample-a-candidates.json[%d]: %w", i, err)
		}
	}
	return ai.Fake{Invoice: &inv, PackingList: &pl, Candidates: cands}, nil
}

// readStrictJSON decodes a file and rejects unknown fields, so a typo in a
// fixture is an error instead of a silently missing value.
func readStrictJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read fake fixture: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if dec.More() {
		return fmt.Errorf("decode %s: more than one JSON value", path)
	}
	return nil
}

func serveLocal(handler http.Handler) error {
	addr := envOr("ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second, // protects against slow-header (Slowloris) clients
		ReadTimeout:       60 * time.Second,
		// Model calls can take 30-90 s and the handler allows them 150 s,
		// so the response deadline must be longer than that.
		WriteTimeout: 3 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	// Stop cleanly on Ctrl+C or when a container platform sends SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc: // e.g. the port is already in use
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// positiveInt reads a whole-number setting of at least 1, or returns
// fallback when the variable is unset (callers pass 0 to mean "the
// package's own default"). A typo such as "25k" or "0" stops the server at
// startup instead of quietly becoming some other limit.
func positiveInt(key string, fallback int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a whole number of at least 1, got %q", key, v)
	}
	return n, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) (string, error) {
	if v := os.Getenv(key); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is required", key)
}
