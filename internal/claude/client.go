// Package claude implements ai.Extractor and ai.Classifier with the Claude
// API. Everything model-specific lives here: prompts, JSON schemas, the
// retry-with-feedback loop, and turning API failures into the ai package's
// errors so HTTP handlers never see SDK types.
package claude

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/domain"
)

const (
	DefaultModel   = "claude-opus-5-5"
	DefaultEffort  = "medium" // also the API's default for this model, but we say so explicitly
	DefaultTimeout = 120 * time.Second

	// One retry after a schema error is usually enough to fix it. More would
	// resend the whole document again for each try, at full price.
	DefaultMaxAttempts = 2

	// DefaultMaxInputTokens caps what one request may send: roughly 15-20
	// pages of invoice. A short PDF file can still hold hundreds of pages,
	// so the file size limit alone doesn't cap the cost of a call.
	DefaultMaxInputTokens = 60000

	// maxTokens caps one answer, thinking included. A few-page invoice
	// needs a few thousand tokens; 16000 leaves headroom while staying a
	// plain (non-streaming) request. An answer that hits it is reported as
	// a document too large to read in one pass, never retried.
	maxTokens = 16000

	// minAttemptTime is the least time that must be left on the caller's
	// deadline to start a model request. An answer takes 30-90 s, so a
	// request started with less would most likely be cut off after the model
	// had already started (and been paid for) the work.
	minAttemptTime = 30 * time.Second

	// retryWait is the pause before the one retry of a request the API
	// turned away with a 429 or 5xx.
	retryWait = 2 * time.Second
)

// missingMessage is the message domain Validate() gives a required value
// that is absent. When it is the only kind of problem left, the model was
// right to leave the values out (the fix-up prompt tells it to), so asking
// again would only repeat the answer and the bill.
const missingMessage = "is required"

// Config configures the client. Zero values mean "use the default".
type Config struct {
	APIKey         string
	Model          string        // default claude-opus-5-5
	Effort         string        // low, medium, high, xhigh or max; default medium
	BaseURL        string        // tests only: points the client at a fake API server
	Timeout        time.Duration // per API request, never past the caller's deadline; default 120 s
	MaxAttempts    int           // model requests per operation, counting the retry after schema errors; default 2
	MaxInputTokens int           // refuse a request with more input tokens than this; default 60000
}

// Client talks to the Claude API. It is safe for concurrent use: it holds
// no per-request state.
type Client struct {
	api            anthropic.Client
	model          string
	effort         anthropic.BetaOutputConfigEffort
	timeout        time.Duration
	maxAttempts    int
	maxInputTokens int64
	log            *slog.Logger

	// Always minAttemptTime and retryWait, except in tests, which shorten
	// them so they don't have to wait for real.
	minAttemptTime time.Duration
	retryWait      time.Duration
}

var (
	_ ai.Extractor  = (*Client)(nil)
	_ ai.Classifier = (*Client)(nil)
)

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("claude: API key is required")
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Effort == "" {
		cfg.Effort = DefaultEffort
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.MaxInputTokens <= 0 {
		cfg.MaxInputTokens = DefaultMaxInputTokens
	}
	// Catch a typo in CLAUDE_EFFORT at start-up, not on the first request.
	switch cfg.Effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return nil, fmt.Errorf("claude: unknown effort %q (want low, medium, high, xhigh or max)", cfg.Effort)
	}

	opts := []option.RequestOption{
		// Use only the key and URL given here. Otherwise the SDK would also
		// read ANTHROPIC_BASE_URL, other credentials and ~/.config profiles
		// from the machine, so where our key gets sent would depend on the
		// environment rather than on our config.
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(cfg.APIKey),
		// The SDK would retry timeouts and dropped connections too, which
		// can mean paying for the whole answer twice. send() decides what is
		// worth retrying instead.
		option.WithMaxRetries(0),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &Client{
		api:            anthropic.NewClient(opts...),
		model:          cfg.Model,
		effort:         anthropic.BetaOutputConfigEffort(cfg.Effort),
		timeout:        cfg.Timeout,
		maxAttempts:    cfg.MaxAttempts,
		maxInputTokens: int64(cfg.MaxInputTokens),
		log:            slog.Default(),
		minAttemptTime: minAttemptTime,
		retryWait:      retryWait,
	}, nil
}

// ---- extraction ----

func (c *Client) ExtractInvoice(ctx context.Context, f ai.File) (domain.InvoiceExtraction, domain.ModelRun, error) {
	doc, err := documentBlock(f)
	if err != nil {
		return domain.InvoiceExtraction{}, domain.ModelRun{Model: c.model}, err
	}
	return ask(ctx, c, request{
		task:    "invoice",
		system:  extractionSystemPrompt,
		schema:  invoiceSchemaJSON,
		content: []anthropic.BetaContentBlockParamUnion{doc, anthropic.NewBetaTextBlock(invoiceUserPrompt)},
	}, func(w invoiceWire) (domain.InvoiceExtraction, domain.ValidationErrors) {
		inv := w.toDomain()
		return inv, asValidationErrors(inv.Validate())
	})
}

func (c *Client) ExtractPackingList(ctx context.Context, f ai.File) (domain.PackingListExtraction, domain.ModelRun, error) {
	doc, err := documentBlock(f)
	if err != nil {
		return domain.PackingListExtraction{}, domain.ModelRun{Model: c.model}, err
	}
	return ask(ctx, c, request{
		task:    "packing_list",
		system:  extractionSystemPrompt,
		schema:  packingListSchemaJSON,
		content: []anthropic.BetaContentBlockParamUnion{doc, anthropic.NewBetaTextBlock(packingListUserPrompt)},
	}, func(w packingListWire) (domain.PackingListExtraction, domain.ValidationErrors) {
		pl := w.toDomain()
		return pl, asValidationErrors(pl.Validate())
	})
}

// documentBlock wraps the file for the API: a PDF as a document block (the
// API reads both its text and its page images), a photo or scan as an image
// block. The content type was sniffed from the bytes on upload.
func documentBlock(f ai.File) (anthropic.BetaContentBlockParamUnion, error) {
	if len(f.Data) == 0 {
		return anthropic.BetaContentBlockParamUnion{}, errors.New("claude: empty file")
	}
	data := base64.StdEncoding.EncodeToString(f.Data)
	switch f.ContentType {
	case "application/pdf":
		return anthropic.NewBetaDocumentBlock(anthropic.BetaBase64PDFSourceParam{Data: data}), nil
	case "image/png":
		return anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
			Data: data, MediaType: anthropic.BetaBase64ImageSourceMediaTypeImagePNG,
		}), nil
	case "image/jpeg":
		return anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
			Data: data, MediaType: anthropic.BetaBase64ImageSourceMediaTypeImageJPEG,
		}), nil
	}
	return anthropic.BetaContentBlockParamUnion{}, fmt.Errorf("claude: unsupported content type %q", f.ContentType)
}

// asValidationErrors turns a Validate() result into the list we feed back
// to the model.
func asValidationErrors(err error) domain.ValidationErrors {
	if err == nil {
		return nil
	}
	var ve domain.ValidationErrors
	if errors.As(err, &ve) {
		return ve
	}
	return domain.ValidationErrors{{Path: "response", Message: err.Error()}}
}

// ---- classification ----

// candidateAnswer is the shape of the classification answer: structured
// outputs need a JSON object at the top level, not a bare list.
type candidateAnswer struct {
	Candidates []ai.Candidate `json:"candidates"`
}

func (c *Client) SuggestCodes(ctx context.Context, lines []ai.Line) ([]ai.Candidate, domain.ModelRun, error) {
	if len(lines) == 0 {
		return nil, domain.ModelRun{Model: c.model}, errors.New("claude: no lines to classify")
	}
	prompt, err := classificationUserPrompt(lines)
	if err != nil {
		return nil, domain.ModelRun{Model: c.model}, err
	}
	asked := map[int]bool{}
	for _, l := range lines {
		asked[l.LineNo] = true
	}

	out, run, err := ask(ctx, c, request{
		task:    "classification",
		system:  classificationSystemPrompt,
		schema:  candidatesSchemaJSON,
		content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(prompt)},
	}, func(a candidateAnswer) ([]ai.Candidate, domain.ValidationErrors) {
		for i := range a.Candidates {
			a.Candidates[i].Confidence = domain.Confidence(enumCase(string(a.Candidates[i].Confidence)))
		}
		return a.Candidates, checkCandidates(a.Candidates, asked)
	})
	if err != nil {
		return nil, run, err
	}

	// Line order, then rank: the order a reviewer reads them in.
	slices.SortStableFunc(out, func(a, b ai.Candidate) int {
		return cmp.Or(cmp.Compare(a.LineNo, b.LineNo), cmp.Compare(a.Rank, b.Rank))
	})
	return out, run, nil
}

var reSubheading = regexp.MustCompile(`^\d{6}$`)

// checkCandidates applies the domain rules to each candidate, plus the rules
// that depend on this request: the line must be one we asked about, the code
// must be a 6-digit HS subheading (the 8-digit national item is the broker's
// choice), each line has at most ranks 1, 2 and 3, each used once, and
// confidence is medium or low (the description is the seller's unverified
// declaration, so no candidate is rated high).
func checkCandidates(cands []ai.Candidate, asked map[int]bool) domain.ValidationErrors {
	var errs domain.ValidationErrors
	type lineRank struct{ line, rank int }
	seen := map[lineRank]bool{}

	for i, cand := range cands {
		p := fmt.Sprintf("candidates[%d]", i)
		s := domain.ClassificationSuggestion{
			LineNo: cand.LineNo, Rank: cand.Rank, HSCode: cand.HSCode,
			Rationale: cand.Rationale, Confidence: cand.Confidence,
		}
		for _, e := range asValidationErrors(s.Validate()) {
			if e.Path == "hs_code" || e.Path == "confidence" {
				continue // reported below with the stricter rules for candidates
			}
			errs = append(errs, domain.FieldError{Path: p + "." + e.Path, Message: e.Message})
		}
		if !reSubheading.MatchString(cand.HSCode) {
			errs = append(errs, domain.FieldError{Path: p + ".hs_code", Message: "must be exactly 6 digits (an HS subheading), with no dots or spaces"})
		}
		if cand.Confidence != domain.ConfidenceMedium && cand.Confidence != domain.ConfidenceLow {
			errs = append(errs, domain.FieldError{Path: p + ".confidence", Message: "must be medium or low: the description is the seller's unverified declaration, so no candidate is rated high"})
		}
		if !asked[cand.LineNo] {
			errs = append(errs, domain.FieldError{Path: p + ".line_no", Message: fmt.Sprintf("line %d is not one of the invoice lines you were given", cand.LineNo)})
		}
		if cand.Rank > 3 {
			errs = append(errs, domain.FieldError{Path: p + ".rank", Message: "must be 1, 2 or 3"})
		}
		key := lineRank{cand.LineNo, cand.Rank}
		if seen[key] {
			errs = append(errs, domain.FieldError{Path: p + ".rank", Message: fmt.Sprintf("rank %d is already used for line %d", cand.Rank, cand.LineNo)})
		}
		seen[key] = true
	}
	return errs
}

// ---- the request loop shared by all three calls ----

// request is one question to the model: a system prompt, the first user
// turn and the JSON schema the answer must follow.
type request struct {
	task    string // for logs only: "invoice", "packing_list" or "classification"
	system  string
	schema  json.RawMessage
	content []anthropic.BetaContentBlockParamUnion
}

// ask sends req and decodes the answer into the wire type W, then read turns
// it into the result T and lists its problems. If there are problems, ask
// tells the model what they are and asks again, up to c.maxAttempts requests
// in total.
//
// A retry continues the same conversation instead of starting over: the
// model's previous answer stays in the history and a new user turn lists
// the problems, so the model fixes what was wrong rather than re-reading
// from scratch and making new mistakes. History is only ever appended to,
// never edited, which also keeps the model's earlier thinking valid.
//
// Every request costs money, so ask stops early when another one can't help:
// the input is too big (checked before the first request), the answer hit
// the output limit, the only problems left are values the document doesn't
// have, or there isn't enough time left for a whole answer.
func ask[W, T any](ctx context.Context, c *Client, req request, read func(W) (T, domain.ValidationErrors)) (T, domain.ModelRun, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: req.system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(req.content...)},
		// Thinking is always on for this model, so there is no thinking
		// setting; effort is how we trade depth against time and cost.
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: c.effort,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: req.schema},
		},
		// If the model's safety classifiers decline (a false positive is
		// possible on any document), the API re-runs the same request on the
		// fallback model Anthropic recommends for that kind of refusal,
		// within this one call. resp.Model then names the model that answered.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}

	var zero T
	run := domain.ModelRun{Model: c.model}
	if err := c.checkInputSize(ctx, req.task, params); err != nil {
		return zero, run, err
	}

	var problems domain.ValidationErrors
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if !c.timeForAttempt(ctx) {
			if len(problems) > 0 {
				return zero, run, &ai.InvalidOutputError{Errs: problems, Run: run}
			}
			return zero, run, fmt.Errorf("claude: less than %s left before the deadline, so no model request was started: %w", c.minAttemptTime, context.DeadlineExceeded)
		}

		run.Attempts = attempt
		start := time.Now()
		resp, err := c.send(ctx, req.task, params)
		if err != nil {
			return zero, run, c.apiError(ctx, err, "task", req.task, "call", "messages", "attempt", attempt)
		}
		// Top-level usage is the model that served the answer. If a fallback
		// ran, the declined attempt's tokens are only logged (see below).
		run.InputTokens += resp.Usage.InputTokens
		run.OutputTokens += resp.Usage.OutputTokens
		if resp.Model != "" {
			run.Model = string(resp.Model)
		}

		var answer T
		problems = nil
		if resp.StopReason != anthropic.BetaStopReasonRefusal && resp.StopReason != anthropic.BetaStopReasonMaxTokens {
			answer, problems = readAnswer(resp, read)
		}
		declinedIn, declinedOut := declinedTokens(resp)
		// Counts and labels only: never the prompt, the document or the answer.
		c.log.Info("claude response",
			"task", req.task,
			"attempt", attempt,
			"model", run.Model,
			"message_id", resp.ID,
			"stop_reason", string(resp.StopReason),
			"refusal_category", string(resp.StopDetails.Category), // empty unless refused
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"declined_input_tokens", declinedIn, // non-zero only when a fallback answered
			"declined_output_tokens", declinedOut,
			"problems", len(problems),
			"duration_ms", time.Since(start).Milliseconds(),
		)

		switch resp.StopReason {
		case anthropic.BetaStopReasonRefusal:
			return zero, run, ai.ErrRefused
		case anthropic.BetaStopReasonMaxTokens:
			// Asking again would hit the same limit, with a longer history.
			return zero, run, fmt.Errorf("%w: the answer reached the %d-token output limit", ai.ErrTooLarge, maxTokens)
		}
		if len(problems) == 0 {
			return answer, run, nil
		}
		if onlyMissing(problems) {
			break
		}

		params.Messages = append(params.Messages,
			resp.ToParam(), // the assistant turn, exactly as received
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(fixPrompt(problems))),
		)
	}
	return zero, run, &ai.InvalidOutputError{Errs: problems, Run: run}
}

// checkInputSize asks the API how many input tokens the request would use
// and refuses it if that is more than c.maxInputTokens. Counting is free,
// and it is the only reliable size check: a small PDF can hold hundreds of
// pages, and page counts can be hidden inside compressed PDF objects.
func (c *Client) checkInputSize(ctx context.Context, task string, params anthropic.BetaMessageNewParams) error {
	countCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	count, err := c.api.Beta.Messages.CountTokens(countCtx, anthropic.BetaMessageCountTokensParams{
		Model:        params.Model,
		System:       anthropic.BetaMessageCountTokensParamsSystemUnion{OfBetaTextBlockArray: params.System},
		Messages:     params.Messages,
		OutputConfig: params.OutputConfig,
	},
		// Counting generates nothing and costs nothing, so unlike a real
		// request it can safely be retried, whatever went wrong.
		option.WithMaxRetries(1),
	)
	if err != nil {
		return c.apiError(ctx, err, "task", task, "call", "count_tokens")
	}

	c.log.Info("claude input counted", "task", task, "input_tokens", count.InputTokens, "limit", c.maxInputTokens)
	if count.InputTokens > c.maxInputTokens {
		return fmt.Errorf("%w: the request has %d input tokens, more than the limit of %d", ai.ErrTooLarge, count.InputTokens, c.maxInputTokens)
	}
	return nil
}

// timeForAttempt reports whether the caller's deadline leaves room for a
// whole model request.
func (c *Client) timeForAttempt(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= c.minAttemptTime
}

// send makes one model request. Its timeout is c.timeout or the time left
// on the caller's deadline, whichever is shorter (context.WithTimeout never
// extends a parent's deadline).
//
// Only a 429 or a 5xx is retried, once: the API turned the request away
// before the model ran, so trying again costs nothing. A timeout or a
// dropped connection is not retried: the model may already have done, and
// billed for, the whole answer.
func (c *Client) send(ctx context.Context, task string, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.api.Beta.Messages.New(ctx, params)
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || !busy(apiErr.StatusCode) {
		return resp, err
	}
	c.log.Warn("claude request turned away, retrying once",
		"task", task, "status", apiErr.StatusCode, "request_id", apiErr.RequestID)
	select {
	case <-time.After(c.retryWait):
	case <-ctx.Done():
		return nil, err
	}
	return c.api.Beta.Messages.New(ctx, params)
}

// busy reports whether an HTTP status means the API is overloaded or failing
// on its side (429 rate limited, 529 overloaded, other 5xx).
func busy(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// onlyMissing reports whether every problem is a required value left out.
func onlyMissing(problems domain.ValidationErrors) bool {
	for _, p := range problems {
		if p.Message != missingMessage {
			return false
		}
	}
	return len(problems) > 0
}

// readAnswer decodes the JSON answer in resp into the wire type W and hands
// it to read. Problems are nil when the answer can be used.
func readAnswer[W, T any](resp *anthropic.BetaMessage, read func(W) (T, domain.ValidationErrors)) (T, domain.ValidationErrors) {
	var wire W
	if err := decodeStrict(answerText(resp), &wire); err != nil {
		var zero T
		return zero, domain.ValidationErrors{{Path: "response", Message: "is not valid JSON for the schema: " + err.Error()}}
	}
	return read(wire)
}

// answerText joins the text blocks of a response. Thinking blocks and any
// fallback marker are skipped; with structured outputs there is one text
// block holding the JSON answer.
func answerText(resp *anthropic.BetaMessage) string {
	var b strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// decodeStrict decodes exactly one JSON value into v and rejects fields the
// Go type doesn't have. The schema already forbids extra fields, so an error
// here means the schema and the wire types have drifted apart.
func decodeStrict(text string, v any) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("no JSON answer was returned")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON answer")
	}
	return nil
}

// declinedTokens counts the tokens of model attempts inside one API call
// that a safety classifier declined before the fallback model answered.
// usage.iterations lists every attempt, the declined one too, while
// top-level usage covers only the attempt that answered, so the difference
// is what was declined. Without a fallback it is zero.
func declinedTokens(resp *anthropic.BetaMessage) (in, out int64) {
	for _, it := range resp.Usage.Iterations {
		in += it.InputTokens
		out += it.OutputTokens
	}
	return max(0, in-resp.Usage.InputTokens), max(0, out-resp.Usage.OutputTokens)
}

// apiError maps a failed API call to the errors the HTTP layer understands.
// logAttrs say which call failed (slog key/value pairs). Messages carry
// status codes and request IDs only: the SDK's own error text includes the
// API's response body, which we don't pass on.
func (c *Client) apiError(ctx context.Context, err error, logAttrs ...any) error {
	// The caller gave up (client went away, or the handler's deadline
	// passed). That is not the model's fault, so pass it through as is.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("claude: %w", ctxErr)
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		c.log.Warn("claude request failed", append(logAttrs,
			"status", apiErr.StatusCode,
			"error_type", string(apiErr.Type()),
			"request_id", apiErr.RequestID,
		)...)
		msg := fmt.Sprintf("claude api returned %d %s (request id %s)", apiErr.StatusCode, apiErr.Type(), apiErr.RequestID)
		if apiErr.StatusCode == http.StatusRequestTimeout || busy(apiErr.StatusCode) {
			// Busy or broken on their side, and still failing after a retry.
			return fmt.Errorf("%w: %s", ai.ErrUnavailable, msg)
		}
		// 400, 401, 403, 413...: a problem with our request or our key.
		// Retrying won't help, so don't call it "unavailable".
		return errors.New(msg)
	}

	// No HTTP response at all: DNS, connection refused, TLS, or our
	// per-request timeout. Network errors name the API URL, never the key.
	// The cause is kept too, so a timeout still reads as
	// context.DeadlineExceeded and the user is told the model took too long.
	c.log.Warn("claude request failed", append(logAttrs, "err", err.Error())...)
	return fmt.Errorf("%w: %w", ai.ErrUnavailable, err)
}
