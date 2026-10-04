package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/domain"
)

// No test here touches the network: fakeAPI stands in for the Messages API
// and its token-counting endpoint.

const testKey = "sk-ant-test-key-do-not-log"

const (
	messagesPath = "/v1/messages"
	countPath    = "/v1/messages/count_tokens"
)

var testPDF = ai.File{ContentType: "application/pdf", Data: []byte("%PDF-1.4 Invoice No: INV-2026-0042 Harbour Ceramics")}

// fakeAPI records each request. It answers /v1/messages with the next
// canned response, repeating the last one once they run out, and
// /v1/messages/count_tokens with a fixed count.
type fakeAPI struct {
	mu        sync.Mutex
	responses []fakeResponse
	count     fakeResponse
	requests  []recorded // every request, in the order received
	answered  int        // /v1/messages requests so far
}

type fakeResponse struct {
	status int
	body   string
	delay  time.Duration // wait this long before answering, unless the client hangs up
}

type recorded struct {
	path   string
	query  string
	header http.Header
	body   map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Reading the body first also lets the server notice when the client hangs up.
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.requests = append(f.requests, recorded{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
	resp := f.count
	if r.URL.Path == messagesPath {
		f.answered++
		resp = f.responses[min(f.answered, len(f.responses))-1]
	}
	f.mu.Unlock()

	if resp.delay > 0 {
		select {
		case <-r.Context().Done(): // the client gave up
			return
		case <-time.After(resp.delay):
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After-Ms", "0") // let the SDK's own retry of a count happen at once
	w.WriteHeader(resp.status)
	io.WriteString(w, resp.body)
}

// calls returns the requests made to one path, in order.
func (f *fakeAPI) calls(path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAPI) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func (f *fakeAPI) setCount(resp fakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count = resp
}

// newTestClient starts a fake API and a client pointed at it. Logs go to
// the returned buffer so tests can check what would reach CloudWatch.
func newTestClient(t *testing.T, cfg Config, responses ...fakeResponse) (*Client, *fakeAPI, *bytes.Buffer) {
	t.Helper()
	fake := &fakeAPI{responses: responses, count: tokenCount(5000)}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg.APIKey = testKey
	cfg.BaseURL = srv.URL
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c.log = slog.New(slog.NewJSONHandler(&logs, nil))
	c.retryWait = time.Millisecond
	return c, fake, &logs
}

func tokenCount(n int) fakeResponse {
	return fakeResponse{status: http.StatusOK, body: `{"input_tokens":` + strconv.Itoa(n) + `}`}
}

// message builds a Messages API response whose answer is text.
func message(stopReason, text string) fakeResponse {
	m := map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "", "signature": "sig"},
			map[string]any{"type": "text", "text": text},
		},
		"stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1000, "output_tokens": 200},
	}
	b, _ := json.Marshal(m)
	return fakeResponse{status: http.StatusOK, body: string(b)}
}

func answer(v any) fakeResponse {
	b, _ := json.Marshal(v)
	return message("end_turn", string(b))
}

func apiError(status int, errType string) fakeResponse {
	return fakeResponse{status: status, body: `{"type":"error","error":{"type":"` + errType + `","message":"test"}}`}
}

// printed is a field the document prints, as the model answers it.
func printed[T any](v T, quote string) wireField[T] {
	return wireField[T]{Found: true, Evidence: wireEvidence{Page: 1, Quote: quote}, Value: v, Confidence: "high"}
}

// notPrinted is a field the document doesn't print. The schema still makes
// the model fill in the other properties; these placeholders are
// deliberately not empty, to show they are thrown away.
func notPrinted[T any](placeholder T) wireField[T] {
	return wireField[T]{Found: false, Evidence: wireEvidence{Page: 0, Quote: "placeholder"}, Value: placeholder, Confidence: "low"}
}

func goodInvoice() invoiceWire {
	return invoiceWire{
		InvoiceNumber: printed("INV-2026-0042", "Invoice No: INV-2026-0042"),
		InvoiceDate:   printed("2026-09-15", "Date: 15 Sep 2026"),
		SellerName:    printed("Harbour Ceramics Co., Ltd.", "Harbour Ceramics Co., Ltd."),
		BuyerName:     notPrinted("x"),
		Currency:      printed("USD", "Currency: USD"),
		Incoterm:      printed("FOB", "FOB Ningbo"),
		LineItems: []invoiceLineWire{{
			LineNo:         1,
			Description:    printed("Porcelain dinner plate, 27 cm, white glazed", "Porcelain dinner plate, 27 cm, white glazed"),
			Quantity:       printed("1200", "1,200 PCS"),
			Unit:           printed("PCS", "1,200 PCS"),
			UnitPrice:      printed("3.50", "3.50"),
			LineTotalMinor: printed(int64(420000), "4,200.00"),
			OriginCountry:  notPrinted("CN"),
		}},
		Charges:    []chargeWire{},
		TotalMinor: printed(int64(730200), "USD 7,302.00"),
	}
}

// badInvoice breaks one rule Validate() checks: lower-case currency.
func badInvoice() invoiceWire {
	inv := goodInvoice()
	inv.Currency = printed("usd", "Currency: usd")
	return inv
}

// get walks a decoded JSON request body, e.g. get(body, "messages", 0, "role").
func get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestNew(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("expected an error without an API key")
	}
	if _, err := New(Config{APIKey: "k", Effort: "extreme"}); err == nil {
		t.Error("expected an error for an unknown effort")
	}
	c, err := New(Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if c.model != DefaultModel || c.effort != DefaultEffort || c.timeout != DefaultTimeout {
		t.Errorf("defaults not applied: %q %q %s", c.model, c.effort, c.timeout)
	}
	if c.maxAttempts != 2 || c.maxInputTokens != 60000 || c.minAttemptTime != 30*time.Second {
		t.Errorf("want 2 attempts, 60000 input tokens, 30 s per attempt; got %d %d %s", c.maxAttempts, c.maxInputTokens, c.minAttemptTime)
	}
}

func TestExtractInvoiceRequestShapeAndResult(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{}, answer(goodInvoice()))

	inv, run, err := c.ExtractInvoice(context.Background(), testPDF)
	if err != nil {
		t.Fatal(err)
	}
	if *inv.InvoiceNumber.Value != "INV-2026-0042" || *inv.TotalMinor.Value != 730200 || *inv.LineItems[0].Quantity.Value != "1200" {
		t.Errorf("unexpected extraction: %+v", inv)
	}
	// found: false becomes a field with no value, confidence or evidence,
	// whatever placeholders the model put in it.
	if !reflect.DeepEqual(inv.BuyerName, domain.Field[string]{}) || !reflect.DeepEqual(inv.LineItems[0].OriginCountry, domain.Field[string]{}) {
		t.Errorf("fields that were not found should be empty: %+v, %+v", inv.BuyerName, inv.LineItems[0].OriginCountry)
	}
	if inv.Charges == nil || len(inv.Charges) != 0 {
		t.Errorf("want an empty (not nil) charges list, got %#v", inv.Charges)
	}
	if run != (domain.ModelRun{Model: "claude-opus-5-5", Attempts: 1, InputTokens: 1000, OutputTokens: 200}) {
		t.Errorf("run = %+v", run)
	}

	// The input is counted first, with the same model, prompt, document and schema.
	all := fake.all()
	if len(all) != 2 || all[0].path != countPath || all[1].path != messagesPath {
		t.Fatalf("want a count then one request, got %d requests", len(all))
	}
	count, req := all[0].body, all[1]
	body := req.body
	if req.header.Get("X-Api-Key") != testKey {
		t.Errorf("api key header %q", req.header.Get("X-Api-Key"))
	}
	if !strings.Contains(req.header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") || body["fallbacks"] != "default" {
		t.Errorf("refusal fallback not requested: beta %q, fallbacks %v", req.header.Get("Anthropic-Beta"), body["fallbacks"])
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"model", body["model"], "claude-opus-5-5"},
		{"max_tokens", body["max_tokens"], 16000.0},
		{"effort", get(body, "output_config", "effort"), "medium"},
		{"format type", get(body, "output_config", "format", "type"), "json_schema"},
		{"schema has a found flag per field", get(body, "output_config", "format", "schema", "properties", "invoice_number", "properties", "found", "type"), "boolean"},
		{"system prompt", get(body, "system", 0, "text"), extractionSystemPrompt},
		{"one message", len(body["messages"].([]any)), 1},
		{"document block first", get(body, "messages", 0, "content", 0, "type"), "document"},
		{"pdf media type", get(body, "messages", 0, "content", 0, "source", "media_type"), "application/pdf"},
		{"pdf data", get(body, "messages", 0, "content", 0, "source", "data"), base64.StdEncoding.EncodeToString(testPDF.Data)},
		{"text block second", get(body, "messages", 0, "content", 1, "text"), invoiceUserPrompt},
		{"count model", count["model"], "claude-opus-5-5"},
		{"count system", get(count, "system", 0, "text"), extractionSystemPrompt},
		{"count messages", jsonEqual(count["messages"], body["messages"]), true},
		{"count schema", jsonEqual(get(count, "output_config", "format"), get(body, "output_config", "format")), true},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s: got %v, want %v", ck.name, ck.got, ck.want)
		}
	}
	// Opus 5.5 rejects these, so they must never be sent.
	for _, key := range []string{"thinking", "temperature", "top_p", "top_k", "tool_choice"} {
		if _, ok := body[key]; ok {
			t.Errorf("request must not set %q", key)
		}
	}
}

func TestEffortFromConfig(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{Effort: "high", Model: "claude-sonnet-5-5"}, answer(goodInvoice()))
	if _, _, err := c.ExtractInvoice(context.Background(), testPDF); err != nil {
		t.Fatal(err)
	}
	body := fake.calls(messagesPath)[0].body
	if get(body, "output_config", "effort") != "high" || body["model"] != "claude-sonnet-5-5" {
		t.Errorf("effort %v, model %v", get(body, "output_config", "effort"), body["model"])
	}
}

func TestImagesUseImageBlocks(t *testing.T) {
	for _, ct := range []string{"image/png", "image/jpeg"} {
		t.Run(ct, func(t *testing.T) {
			c, fake, _ := newTestClient(t, Config{}, answer(goodInvoice()))
			if _, _, err := c.ExtractInvoice(context.Background(), ai.File{ContentType: ct, Data: []byte("img")}); err != nil {
				t.Fatal(err)
			}
			block := get(fake.calls(messagesPath)[0].body, "messages", 0, "content", 0)
			if get(block, "type") != "image" || get(block, "source", "media_type") != ct || get(block, "source", "type") != "base64" {
				t.Errorf("unexpected block %v", block)
			}
		})
	}
}

func TestUnsupportedFileIsNotSent(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{}, answer(goodInvoice()))
	for _, f := range []ai.File{{ContentType: "text/html", Data: []byte("<html>")}, {ContentType: "application/pdf"}} {
		if _, _, err := c.ExtractInvoice(context.Background(), f); err == nil {
			t.Errorf("%q: expected an error", f.ContentType)
		}
	}
	if n := len(fake.all()); n != 0 {
		t.Errorf("want no requests, got %d", n)
	}
}

func TestExtractPackingList(t *testing.T) {
	pl := packingListWire{
		PackingListNumber:     notPrinted("PL-?"),
		InvoiceRef:            printed("INV-2026-0042", "Invoice Ref: INV-2026-0042"),
		TotalPackages:         printed(int64(84), "84 CTNS"),
		TotalGrossWeightGrams: printed(int64(936000), "936.00 kg"),
		LineItems: []packingLineWire{{
			LineNo:           1,
			Description:      printed("Ceramic coffee mug, 350 ml", "Ceramic coffee mug, 350 ml"),
			Quantity:         printed("780", "780 PCS"),
			Unit:             printed("PCS", "780 PCS"),
			Packages:         printed(int64(20), "20 CTNS"),
			GrossWeightGrams: printed(int64(210000), "210.00 kg"),
		}},
	}
	c, fake, _ := newTestClient(t, Config{}, answer(pl))
	got, _, err := c.ExtractPackingList(context.Background(), testPDF)
	if err != nil {
		t.Fatal(err)
	}
	if *got.TotalPackages.Value != 84 || *got.LineItems[0].Quantity.Value != "780" || got.PackingListNumber.Present() {
		t.Errorf("unexpected extraction: %+v", got)
	}
	body := fake.calls(messagesPath)[0].body
	if get(body, "output_config", "format", "schema", "properties", "total_packages", "type") != "object" {
		t.Error("packing list request did not use the packing list schema")
	}
	if get(body, "messages", 0, "content", 1, "text") != packingListUserPrompt {
		t.Error("packing list request did not use the packing list prompt")
	}
}

// Before the review, currency was required while the prompt told the model
// to leave a bare "$" out, so such an invoice could never be extracted.
func TestCurrencyMayBeMissing(t *testing.T) {
	inv := goodInvoice()
	inv.Currency = notPrinted("")
	c, fake, _ := newTestClient(t, Config{}, answer(inv))

	got, _, err := c.ExtractInvoice(context.Background(), testPDF)
	if err != nil {
		t.Fatal(err)
	}
	if got.Currency.Present() {
		t.Errorf("currency should be empty, got %+v", got.Currency)
	}
	if n := len(fake.calls(messagesPath)); n != 1 {
		t.Errorf("want 1 request, got %d", n)
	}
}

// Goods sold by weight or length have decimal quantities, and freight,
// insurance and discount rows have their own place in the answer.
func TestChargesAndDecimalQuantities(t *testing.T) {
	inv := goodInvoice()
	inv.LineItems[0].Quantity = printed("2450.75", "2,450.75 KG")
	inv.LineItems[0].Unit = printed("KG", "2,450.75 KG")
	inv.Charges = []chargeWire{
		// The capital F is on purpose: structured outputs don't guarantee
		// the case of an enum value.
		{Kind: "Freight", Label: printed("Ocean freight", "Ocean freight 450.00"), AmountMinor: printed(int64(45000), "450.00")},
		{Kind: "discount", Label: printed("Early payment discount", "Early payment discount -180.00"), AmountMinor: printed(int64(18000), "-180.00")},
	}
	c, _, _ := newTestClient(t, Config{}, answer(inv))

	got, _, err := c.ExtractInvoice(context.Background(), testPDF)
	if err != nil {
		t.Fatal(err)
	}
	if q := *got.LineItems[0].Quantity.Value; q != "2450.75" {
		t.Errorf("quantity = %q, want the printed decimal", q)
	}
	want := []domain.InvoiceCharge{
		{Kind: domain.ChargeFreight, Label: domain.Of("Ocean freight", domain.ConfidenceHigh, 1, "Ocean freight 450.00"), AmountMinor: domain.Of(int64(45000), domain.ConfidenceHigh, 1, "450.00")},
		{Kind: domain.ChargeDiscount, Label: domain.Of("Early payment discount", domain.ConfidenceHigh, 1, "Early payment discount -180.00"), AmountMinor: domain.Of(int64(18000), domain.ConfidenceHigh, 1, "-180.00")},
	}
	if !reflect.DeepEqual(got.Charges, want) {
		t.Errorf("charges = %+v", got.Charges)
	}
}

func TestWireFieldToDomain(t *testing.T) {
	tests := []struct {
		name string
		in   wireField[string]
		want domain.Field[string]
	}{
		{"not found drops the placeholders", wireField[string]{Found: false, Evidence: wireEvidence{Page: 3, Quote: "q"}, Value: "v", Confidence: "high"}, domain.Field[string]{}},
		{"found", printed("USD", "Currency: USD"), domain.Of("USD", domain.ConfidenceHigh, 1, "Currency: USD")},
		{"enum case is ignored", wireField[string]{Found: true, Evidence: wireEvidence{Page: 1, Quote: "q"}, Value: "v", Confidence: "Medium"}, domain.Of("v", domain.ConfidenceMedium, 1, "q")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.toDomain(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A bad first answer is fed back in the same conversation and fixed on the
// second attempt.
func TestRetryContinuesConversation(t *testing.T) {
	tests := []struct {
		name     string
		first    fakeResponse
		wantPath string // must appear in the follow-up turn
	}{
		{"schema error", answer(badInvoice()), "currency"},
		{"not JSON", message("end_turn", `{"invoice_number": `), "response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fake, _ := newTestClient(t, Config{}, tc.first, answer(goodInvoice()))

			inv, run, err := c.ExtractInvoice(context.Background(), testPDF)
			if err != nil {
				t.Fatal(err)
			}
			if *inv.Currency.Value != "USD" {
				t.Errorf("want the corrected answer, got currency %q", *inv.Currency.Value)
			}
			if run.Attempts != 2 || run.InputTokens != 2000 || run.OutputTokens != 400 {
				t.Errorf("run = %+v", run)
			}

			calls := fake.calls(messagesPath)
			if len(calls) != 2 {
				t.Fatalf("want 2 requests, got %d", len(calls))
			}
			if n := len(fake.calls(countPath)); n != 1 {
				t.Errorf("the input should be counted once, got %d counts", n)
			}
			first, second := calls[0].body["messages"].([]any), calls[1].body["messages"].([]any)
			if len(second) != 3 {
				t.Fatalf("want user, assistant, user; got %d messages", len(second))
			}
			// Append-only: the first turn is resent unchanged.
			if !jsonEqual(first[0], second[0]) {
				t.Error("first user turn changed on retry")
			}
			// The model's own previous answer is the assistant turn, as received.
			var firstAnswer map[string]any
			json.Unmarshal([]byte(tc.first.body), &firstAnswer)
			if get(second, 1, "role") != "assistant" || get(second, 1, "content", 1, "text") != get(firstAnswer, "content", 1, "text") {
				t.Errorf("assistant turn not echoed: %v", second[1])
			}
			if get(second, 1, "content", 0, "signature") != "sig" {
				t.Error("thinking block not echoed unchanged")
			}
			fix, _ := get(second, 2, "content", 0, "text").(string)
			if get(second, 2, "role") != "user" || !strings.Contains(fix, tc.wantPath) {
				t.Errorf("follow-up turn should mention %q, got %q", tc.wantPath, fix)
			}
		})
	}
}

// An answer cut off at max_tokens would be cut off again, with a longer
// history, so it is reported as too large at once instead of retried.
func TestMaxTokensIsTooLarge(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{}, message("max_tokens", `{"invoice_number": {"found": true, "evidence": `), answer(goodInvoice()))

	_, run, err := c.ExtractInvoice(context.Background(), testPDF)
	if !errors.Is(err, ai.ErrTooLarge) {
		t.Fatalf("want ai.ErrTooLarge, got %v", err)
	}
	if n := len(fake.calls(messagesPath)); n != 1 {
		t.Errorf("want 1 request, got %d", n)
	}
	if run.Attempts != 1 || run.InputTokens != 1000 {
		t.Errorf("run = %+v", run)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{}, answer(badInvoice()))

	_, run, err := c.ExtractInvoice(context.Background(), testPDF)
	var invalid *ai.InvalidOutputError
	if !errors.As(err, &invalid) {
		t.Fatalf("want *ai.InvalidOutputError, got %v", err)
	}
	if run.Attempts != 2 || invalid.Run.Attempts != 2 || run.InputTokens != 2000 || run.OutputTokens != 400 {
		t.Errorf("run = %+v, error run = %+v", run, invalid.Run)
	}
	if len(invalid.Errs) == 0 || invalid.Errs[0].Path != "currency" {
		t.Errorf("want the currency problem reported, got %v", invalid.Errs)
	}
	if n := len(fake.calls(messagesPath)); n != 2 {
		t.Errorf("want 2 requests by default, got %d", n)
	}
}

func TestMaxAttemptsFromConfig(t *testing.T) {
	for _, n := range []int{1, 3} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			c, fake, _ := newTestClient(t, Config{MaxAttempts: n}, answer(badInvoice()))
			var invalid *ai.InvalidOutputError
			if _, _, err := c.ExtractInvoice(context.Background(), testPDF); !errors.As(err, &invalid) {
				t.Fatalf("want *ai.InvalidOutputError, got %v", err)
			}
			calls := fake.calls(messagesPath)
			if len(calls) != n {
				t.Fatalf("want %d requests, got %d", n, len(calls))
			}
			// Each retry adds exactly one assistant turn and one user turn.
			if got := len(calls[n-1].body["messages"].([]any)); got != 2*n-1 {
				t.Errorf("last request: want %d messages, got %d", 2*n-1, got)
			}
		})
	}
}

// When the only problems are required values the document doesn't print,
// the model was right to leave them out, and asking again would only pay
// for the same answer.
func TestStopsWhenOnlyRequiredValuesAreMissing(t *testing.T) {
	missingDate := goodInvoice()
	missingDate.InvoiceDate = notPrinted("2026-01-01")
	missingAndWrong := badInvoice()
	missingAndWrong.InvoiceDate = notPrinted("2026-01-01")

	tests := []struct {
		name         string
		answer       invoiceWire
		wantRequests int
	}{
		{"only a missing value", missingDate, 1},
		{"a missing value and a fixable one", missingAndWrong, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fake, _ := newTestClient(t, Config{}, answer(tc.answer))
			_, _, err := c.ExtractInvoice(context.Background(), testPDF)
			var invalid *ai.InvalidOutputError
			if !errors.As(err, &invalid) {
				t.Fatalf("want *ai.InvalidOutputError, got %v", err)
			}
			if !slicesContain(invalid.Errs, domain.FieldError{Path: "invoice_date", Message: missingMessage}) {
				t.Errorf("want invoice_date reported as missing, got %v", invalid.Errs)
			}
			if n := len(fake.calls(messagesPath)); n != tc.wantRequests {
				t.Errorf("want %d requests, got %d", tc.wantRequests, n)
			}
		})
	}
}

// onlyMissing depends on the exact message the domain uses.
func TestMissingMessageMatchesDomain(t *testing.T) {
	errs := asValidationErrors(domain.InvoiceExtraction{}.Validate())
	if !slicesContain(errs, domain.FieldError{Path: "invoice_number", Message: missingMessage}) {
		t.Errorf("domain no longer says %q for a missing value: %v", missingMessage, errs)
	}
}

func slicesContain(errs domain.ValidationErrors, want domain.FieldError) bool {
	for _, e := range errs {
		if e == want {
			return true
		}
	}
	return false
}

// The input is counted (free) before anything is sent to the model, and a
// request over the limit is refused without a paid call.
func TestInputSizeLimit(t *testing.T) {
	tests := []struct {
		name         string
		tokens       int
		wantTooLarge bool
	}{
		{"at the limit", 1000, false},
		{"over the limit", 1001, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fake, logs := newTestClient(t, Config{MaxInputTokens: 1000}, answer(goodInvoice()))
			fake.setCount(tokenCount(tc.tokens))

			_, run, err := c.ExtractInvoice(context.Background(), testPDF)
			if got := errors.Is(err, ai.ErrTooLarge); got != tc.wantTooLarge {
				t.Fatalf("errors.Is(err, ErrTooLarge) = %v, want %v (err: %v)", got, tc.wantTooLarge, err)
			}
			wantRequests := 1
			if tc.wantTooLarge {
				wantRequests = 0
				if run.Attempts != 0 {
					t.Errorf("run = %+v", run)
				}
			}
			if n := len(fake.calls(messagesPath)); n != wantRequests {
				t.Errorf("want %d model requests, got %d", wantRequests, n)
			}
			if !strings.Contains(logs.String(), `"input_tokens":`+strconv.Itoa(tc.tokens)) {
				t.Errorf("the count should be logged:\n%s", logs)
			}
		})
	}
}

// If the input can't be counted, nothing is sent: the size check is what
// caps the cost of a call.
func TestCountFailureSendsNothing(t *testing.T) {
	tests := []struct {
		name            string
		resp            fakeResponse
		wantUnavailable bool
		wantCounts      int // counting is free, so the SDK may retry it once
	}{
		{"529", apiError(529, "overloaded_error"), true, 2},
		{"400", apiError(400, "invalid_request_error"), false, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fake, _ := newTestClient(t, Config{}, answer(goodInvoice()))
			fake.setCount(tc.resp)

			_, _, err := c.ExtractInvoice(context.Background(), testPDF)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, ai.ErrUnavailable); got != tc.wantUnavailable {
				t.Errorf("errors.Is(err, ErrUnavailable) = %v, want %v (err: %v)", got, tc.wantUnavailable, err)
			}
			if n := len(fake.calls(countPath)); n != tc.wantCounts {
				t.Errorf("want %d counts, got %d", tc.wantCounts, n)
			}
			if n := len(fake.calls(messagesPath)); n != 0 {
				t.Errorf("want no model requests, got %d", n)
			}
		})
	}
}

// No request starts without enough time left on the caller's deadline for
// a whole answer.
func TestNoAttemptWithoutEnoughTimeLeft(t *testing.T) {
	t.Run("before the first attempt", func(t *testing.T) {
		c, fake, _ := newTestClient(t, Config{}, answer(goodInvoice()))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // less than 30 s
		defer cancel()

		_, _, err := c.ExtractInvoice(ctx, testPDF)
		var invalid *ai.InvalidOutputError
		if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &invalid) {
			t.Fatalf("want context.DeadlineExceeded, got %v", err)
		}
		if n := len(fake.calls(messagesPath)); n != 0 {
			t.Errorf("want no model requests, got %d", n)
		}
	})

	t.Run("after a failed attempt", func(t *testing.T) {
		slowBad := answer(badInvoice())
		slowBad.delay = 600 * time.Millisecond
		c, fake, _ := newTestClient(t, Config{}, slowBad, answer(goodInvoice()))
		c.minAttemptTime = time.Second
		// Enough for the first attempt, but after it less than a second is left.
		ctx, cancel := context.WithTimeout(context.Background(), 1400*time.Millisecond)
		defer cancel()

		_, _, err := c.ExtractInvoice(ctx, testPDF)
		var invalid *ai.InvalidOutputError
		if !errors.As(err, &invalid) {
			t.Fatalf("want the first answer's problems as *ai.InvalidOutputError, got %v", err)
		}
		if invalid.Run.Attempts != 1 || len(invalid.Errs) == 0 || invalid.Errs[0].Path != "currency" {
			t.Errorf("error = %+v", invalid)
		}
		if n := len(fake.calls(messagesPath)); n != 1 {
			t.Errorf("want 1 request, got %d", n)
		}
	})
}

// A request that runs past the per-request timeout is not retried: the
// model may already have done (and billed for) the work. It reads as both
// "unavailable" and "deadline exceeded", so the user is told it took too long.
func TestTimeoutIsNotRetried(t *testing.T) {
	slow := answer(goodInvoice())
	slow.delay = 5 * time.Second
	c, fake, _ := newTestClient(t, Config{Timeout: 50 * time.Millisecond}, slow)

	_, _, err := c.ExtractInvoice(context.Background(), testPDF)
	if !errors.Is(err, ai.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ai.ErrUnavailable and context.DeadlineExceeded, got %v", err)
	}
	if n := len(fake.calls(messagesPath)); n != 1 {
		t.Errorf("a timed-out request must not be resent; got %d requests", n)
	}
}

// Each request's timeout is cut short by the caller's deadline.
func TestTimeoutStopsAtCallersDeadline(t *testing.T) {
	slow := answer(goodInvoice())
	slow.delay = 5 * time.Second
	c, fake, _ := newTestClient(t, Config{Timeout: time.Minute}, slow)
	c.minAttemptTime = 0
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := c.ExtractInvoice(ctx, testPDF)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("want the caller's context.DeadlineExceeded only, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s; the caller's deadline was 300 ms", d)
	}
	if n := len(fake.calls(messagesPath)); n != 1 {
		t.Errorf("want 1 request, got %d", n)
	}
}

func TestAPIErrors(t *testing.T) {
	tests := []struct {
		name            string
		resp            fakeResponse
		wantUnavailable bool
		wantRequests    int // a 429 or 5xx is retried once
	}{
		{"500", apiError(500, "api_error"), true, 2},
		{"529 overloaded", apiError(529, "overloaded_error"), true, 2},
		{"429", apiError(429, "rate_limit_error"), true, 2},
		{"400", apiError(400, "invalid_request_error"), false, 1},
		{"401", apiError(401, "authentication_error"), false, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fake, _ := newTestClient(t, Config{}, tc.resp)
			_, _, err := c.ExtractInvoice(context.Background(), testPDF)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, ai.ErrUnavailable); got != tc.wantUnavailable {
				t.Errorf("errors.Is(err, ErrUnavailable) = %v, want %v (err: %v)", got, tc.wantUnavailable, err)
			}
			if n := len(fake.calls(messagesPath)); n != tc.wantRequests {
				t.Errorf("want %d requests, got %d", tc.wantRequests, n)
			}
		})
	}
}

// A request the API turned away is retried, and the retry's answer is used.
// The retry is not a second attempt at the answer.
func TestBusyRetrySucceeds(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{}, apiError(529, "overloaded_error"), answer(goodInvoice()))
	_, run, err := c.ExtractInvoice(context.Background(), testPDF)
	if err != nil {
		t.Fatal(err)
	}
	if run.Attempts != 1 || len(fake.calls(messagesPath)) != 2 {
		t.Errorf("run = %+v, requests = %d", run, len(fake.calls(messagesPath)))
	}
}

func TestNetworkErrorIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens there any more

	c, err := New(Config{APIKey: testKey, BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	c.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	if _, _, err := c.ExtractInvoice(context.Background(), testPDF); !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("want ai.ErrUnavailable, got %v", err)
	}
}

// If the caller cancels (the browser went away), that is reported as a
// cancellation, not as the model being unavailable.
func TestCallerCancellationPassesThrough(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{}, answer(goodInvoice()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := c.ExtractInvoice(ctx, testPDF)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("want context.Canceled only, got %v", err)
	}
	if n := len(fake.all()); n != 0 {
		t.Errorf("want no requests, got %d", n)
	}
}

func TestRefusal(t *testing.T) {
	refusal := fakeResponse{status: http.StatusOK, body: `{"id":"msg_r","type":"message","role":"assistant","model":"claude-opus-5-5",
		"content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},
		"usage":{"input_tokens":900,"output_tokens":0}}`}
	c, fake, logs := newTestClient(t, Config{}, refusal)

	_, run, err := c.ExtractInvoice(context.Background(), testPDF)
	if !errors.Is(err, ai.ErrRefused) {
		t.Fatalf("want ai.ErrRefused, got %v", err)
	}
	if !strings.Contains(logs.String(), `"refusal_category":"cyber"`) {
		t.Errorf("refusal category should be logged:\n%s", logs)
	}
	if run.Attempts != 1 || run.InputTokens != 900 {
		t.Errorf("run = %+v", run)
	}
	if n := len(fake.calls(messagesPath)); n != 1 {
		t.Errorf("a refusal must not be retried; got %d requests", n)
	}
}

// When the server-side fallback answers, the run records the model that
// answered and that model's tokens only. The declined attempt's tokens are
// logged as counts.
func TestFallbackModelAndTokensAreRecorded(t *testing.T) {
	inv, _ := json.Marshal(goodInvoice())
	resp := map[string]any{
		"id": "msg_f", "type": "message", "role": "assistant", "model": "claude-opus-4-8",
		"content": []any{
			map[string]any{"type": "fallback", "from": map[string]any{"model": "claude-opus-5-5"}, "to": map[string]any{"model": "claude-opus-4-8"}},
			map[string]any{"type": "text", "text": string(inv)},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens": 1000, "output_tokens": 200,
			"iterations": []any{
				map[string]any{"type": "message", "model": "claude-opus-5-5", "input_tokens": 1000, "output_tokens": 30},
				map[string]any{"type": "fallback_message", "model": "claude-opus-4-8", "input_tokens": 1000, "output_tokens": 200},
			},
		},
	}
	body, _ := json.Marshal(resp)
	c, _, logs := newTestClient(t, Config{}, fakeResponse{status: http.StatusOK, body: string(body)})

	_, run, err := c.ExtractInvoice(context.Background(), testPDF)
	if err != nil {
		t.Fatal(err)
	}
	if run != (domain.ModelRun{Model: "claude-opus-4-8", Attempts: 1, InputTokens: 1000, OutputTokens: 200}) {
		t.Errorf("run = %+v", run)
	}
	for _, s := range []string{`"declined_input_tokens":1000`, `"declined_output_tokens":30`} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("log should contain %s:\n%s", s, logs)
		}
	}
}

// ---- classification ----

var testLines = []ai.Line{
	{LineNo: 1, Description: "Porcelain dinner plate, 27 cm, white glazed", Unit: "PCS"},
	{LineNo: 2, Description: "Stoneware coffee mug, 350 ml", Unit: "PCS"},
}

func candidate(line, rank int, code string) ai.Candidate {
	return ai.Candidate{
		LineNo: line, Rank: rank, HSCode: code,
		Rationale:   "Heading 6911 covers tableware of porcelain or china (GIR 1); subheading 6911.10 tableware (GIR 6).",
		Confidence:  domain.ConfidenceMedium,
		MissingInfo: []string{"body material: porcelain or stoneware"},
	}
}

func TestSuggestCodes(t *testing.T) {
	// Out of order on purpose: the client returns line order, then rank.
	capitalised := candidate(2, 1, "691200")
	capitalised.Confidence = "Medium" // structured outputs don't guarantee enum case
	resp := answer(candidateAnswer{Candidates: []ai.Candidate{
		capitalised, candidate(1, 2, "691200"), candidate(1, 1, "691110"),
	}})
	c, fake, _ := newTestClient(t, Config{}, resp)

	got, run, err := c.SuggestCodes(context.Background(), testLines)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].LineNo != 1 || got[0].Rank != 1 || got[1].Rank != 2 || got[2].LineNo != 2 {
		t.Errorf("unexpected order: %+v", got)
	}
	if got[2].Confidence != domain.ConfidenceMedium {
		t.Errorf("confidence %q should be read as medium", got[2].Confidence)
	}
	if run.Attempts != 1 || run.Model != "claude-opus-5-5" {
		t.Errorf("run = %+v", run)
	}

	body := fake.calls(messagesPath)[0].body
	if get(body, "system", 0, "text") != classificationSystemPrompt {
		t.Error("classification system prompt not used")
	}
	if !jsonEqual(get(body, "output_config", "format", "schema", "properties", "candidates", "items", "properties", "confidence", "enum"), []string{"medium", "low"}) {
		t.Error("candidates schema must offer only medium and low confidence")
	}
	text, _ := get(body, "messages", 0, "content", 0, "text").(string)
	if !strings.Contains(text, `"description": "Stoneware coffee mug, 350 ml"`) || !strings.Contains(text, `"unit": "PCS"`) {
		t.Errorf("lines not sent as JSON data: %s", text)
	}
	if strings.Contains(text, "origin") {
		t.Errorf("origin does not change an HS subheading and must not be sent: %s", text)
	}
}

func TestSuggestCodesRetriesUnknownLine(t *testing.T) {
	first := answer(candidateAnswer{Candidates: []ai.Candidate{candidate(1, 1, "691110"), candidate(9, 1, "691200")}})
	second := answer(candidateAnswer{Candidates: []ai.Candidate{candidate(1, 1, "691110")}})
	c, fake, _ := newTestClient(t, Config{}, first, second)

	got, run, err := c.SuggestCodes(context.Background(), testLines)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || run.Attempts != 2 {
		t.Errorf("got %+v, run %+v", got, run)
	}
	fix, _ := get(fake.calls(messagesPath)[1].body, "messages", 2, "content", 0, "text").(string)
	if !strings.Contains(fix, "candidates[1].line_no") {
		t.Errorf("follow-up turn should name the unknown line, got %q", fix)
	}
}

func TestSuggestCodesNeedsLines(t *testing.T) {
	c, fake, _ := newTestClient(t, Config{}, answer(candidateAnswer{}))
	if _, _, err := c.SuggestCodes(context.Background(), nil); err == nil {
		t.Error("expected an error for no lines")
	}
	if n := len(fake.all()); n != 0 {
		t.Errorf("want no requests, got %d", n)
	}
}

func TestCheckCandidates(t *testing.T) {
	asked := map[int]bool{1: true, 2: true}
	tests := []struct {
		name     string
		mutate   func(*ai.Candidate)
		wantPath string // "" means no problems
	}{
		{"valid", func(*ai.Candidate) {}, ""},
		{"low confidence", func(c *ai.Candidate) { c.Confidence = domain.ConfidenceLow }, ""},
		{"leading zero kept", func(c *ai.Candidate) { c.HSCode = "090121" }, ""},
		{"unknown line", func(c *ai.Candidate) { c.LineNo = 7 }, "candidates[1].line_no"},
		{"8-digit national item", func(c *ai.Candidate) { c.HSCode = "69111000" }, "candidates[1].hs_code"},
		{"dotted code", func(c *ai.Candidate) { c.HSCode = "6911.10" }, "candidates[1].hs_code"},
		{"rank 4", func(c *ai.Candidate) { c.Rank = 4 }, "candidates[1].rank"},
		{"duplicate rank", func(c *ai.Candidate) { c.Rank = 1 }, "candidates[1].rank"},
		{"high confidence", func(c *ai.Candidate) { c.Confidence = domain.ConfidenceHigh }, "candidates[1].confidence"},
		{"percentage confidence", func(c *ai.Candidate) { c.Confidence = "92%" }, "candidates[1].confidence"},
		{"empty rationale", func(c *ai.Candidate) { c.Rationale = " " }, "candidates[1].rationale"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cands := []ai.Candidate{candidate(1, 1, "691110"), candidate(1, 2, "691200")}
			tc.mutate(&cands[1])
			errs := checkCandidates(cands, asked)
			if tc.wantPath == "" {
				if errs != nil {
					t.Fatalf("expected no problems, got %v", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Path == tc.wantPath {
					return
				}
			}
			t.Fatalf("expected a problem at %q, got %v", tc.wantPath, errs)
		})
	}
}

// ---- prompts ----

// The prompts are the other half of the schema: these are the rules the
// review found missing or contradictory, so a later edit can't quietly
// undo them.
func TestPromptsStateTheRules(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		phrase string
		want   bool
	}{
		{"not printed means found false", extractionSystemPrompt, "set found to false", true},
		{"line_no is the position among goods rows", extractionSystemPrompt, "1-based position among the goods rows", true},
		{"printed item numbers are ignored", extractionSystemPrompt, "Ignore printed item numbers", true},
		{"charges have their own place", extractionSystemPrompt, "go in charges", true},
		{"quantities are decimals", extractionSystemPrompt, `"2450.75"`, true},
		{"package totals count one kind", extractionSystemPrompt, "same kind of package as the rows", true},
		{"a bare $ is not a currency", extractionSystemPrompt, "A symbol alone, such as $, is not a currency", true},
		{"a date conversion is not medium", extractionSystemPrompt, "a date format conversion", false},
		{"candidates are never high", classificationSystemPrompt, "no candidate is ever rated high", true},
		{"old high rule is gone", classificationSystemPrompt, "high only when", false},
		{"origin is not sent", classificationSystemPrompt, "Origin is given only as context", false},
		{"wording rule", classificationSystemPrompt, "candidate codes for a licensed customs broker to review", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Contains(tc.prompt, tc.phrase); got != tc.want {
				t.Errorf("prompt contains %q = %v, want %v", tc.phrase, got, tc.want)
			}
		})
	}
}

// ---- logging ----

// Logs go to CloudWatch, so they must never hold the API key, prompts,
// document contents or model output: only counts and labels.
func TestLogsHoldNoSecretsOrContent(t *testing.T) {
	c, _, logs := newTestClient(t, Config{}, answer(badInvoice()), answer(goodInvoice()), apiError(401, "authentication_error"))

	if _, _, err := c.ExtractInvoice(context.Background(), testPDF); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.SuggestCodes(context.Background(), testLines); err == nil {
		t.Fatal("expected the 401 to fail")
	}

	out := logs.String()
	forbidden := []string{
		testKey,
		extractionSystemPrompt[:60],
		classificationSystemPrompt[:60],
		invoiceUserPrompt,
		"did not pass validation", // the fix-up prompt
		base64.StdEncoding.EncodeToString(testPDF.Data)[:20], // the document
		"INV-2026-0042", "Harbour Ceramics", "730200", // model output
		"Porcelain dinner plate", "Stoneware coffee mug", // line descriptions
	}
	for _, s := range forbidden {
		if strings.Contains(out, s) {
			t.Errorf("log contains %q:\n%s", s, out)
		}
	}
	// And the useful facts are there.
	for _, s := range []string{`"task":"invoice"`, `"attempt":2`, `"input_tokens":1000`, `"input_tokens":5000`, `"status":401`, `"call":"messages"`} {
		if !strings.Contains(out, s) {
			t.Errorf("log should contain %s:\n%s", s, out)
		}
	}
}
