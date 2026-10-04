package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/domain"
)

func TestModelNotConfigured(t *testing.T) {
	e := newEnv(t) // no extractor, no classifier
	shipID := e.createShipment("JOB-020")
	doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)
	e.submitManual(doc.ID, invoiceFixture())

	for _, path := range []string{"/v1/documents/" + doc.ID + "/extract", "/v1/shipments/" + shipID + "/suggestions"} {
		r := e.mustDo("POST", path, nil, http.StatusServiceUnavailable)
		if r.errCode(t) != "model_not_configured" {
			t.Fatalf("%s: got %s", path, r.raw)
		}
	}
}

func TestModelQuotaExhausted(t *testing.T) {
	e := newEnv(t, withModel(newFakeModel()), fixedClock(noon), func(c *Config) { c.ModelCallsPerDay = 1 })
	shipID := e.createShipment("JOB-021")
	doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)

	e.mustDo("POST", "/v1/documents/"+doc.ID+"/extract", nil, http.StatusCreated)
	// The budget is global and shared by both model routes.
	for _, path := range []string{"/v1/documents/" + doc.ID + "/extract", "/v1/shipments/" + shipID + "/suggestions"} {
		r := e.mustDo("POST", path, nil, http.StatusTooManyRequests)
		if r.errCode(t) != "model_quota_exhausted" {
			t.Fatalf("%s: got %s", path, r.raw)
		}
		if got := r.header.Get("Retry-After"); got != "43200" { // 12 h until midnight UTC
			t.Fatalf("Retry-After = %q", got)
		}
	}
}

func TestModelErrors(t *testing.T) {
	run := domain.ModelRun{Model: "stub-model", Attempts: 3, InputTokens: 1000, OutputTokens: 200}
	invalid := &ai.InvalidOutputError{
		Errs: domain.ValidationErrors{{Path: "invoice_number", Message: "is required"}},
		Run:  run,
	}
	tests := []struct {
		name   string
		model  stubModel
		status int
		code   string
		reason string
	}{
		{"refused", stubModel{run: run, err: ai.ErrRefused}, http.StatusUnprocessableEntity, "model_refused", "refused"},
		{"invalid output", stubModel{err: invalid}, http.StatusUnprocessableEntity, "validation_failed", "invalid_output"},
		{"unavailable", stubModel{err: fmt.Errorf("after retries: %w", ai.ErrUnavailable)}, http.StatusBadGateway, "model_failed", "unavailable"},
		{"timeout", stubModel{err: context.DeadlineExceeded}, http.StatusBadGateway, "model_failed", "timeout"},
		{"too large", stubModel{run: run, err: fmt.Errorf("count tokens: %w", ai.ErrTooLarge)},
			http.StatusUnprocessableEntity, "document_too_large", "too_large"},
		// The extractor broke its promise and returned invalid data without an
		// error: it must still not be stored.
		{"invalid data without error", stubModel{run: run}, http.StatusUnprocessableEntity, "validation_failed", "invalid_output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, withModel(tt.model), fixedClock(noon))
			shipID := e.createShipment("JOB-022")
			doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)

			r := e.mustDo("POST", "/v1/documents/"+doc.ID+"/extract", nil, tt.status)
			if got := r.errCode(t); got != tt.code {
				t.Fatalf("code = %s, want %s (%s)", got, tt.code, r.raw)
			}
			if tt.code == "validation_failed" && !bytes.Contains(r.raw, []byte(`"path":"invoice_number"`)) {
				t.Fatalf("validation details missing: %s", r.raw)
			}
			exts, _ := e.store.ListExtractions(t.Context(), doc.ID)
			if len(exts) != 0 {
				t.Fatal("a failed model call must not store an extraction")
			}
			events := e.audit(shipID)
			last := events[len(events)-1]
			if last.Action != "extraction.model_failed" || last.Detail["reason"] != tt.reason {
				t.Fatalf("last audit = %+v", last)
			}
			// The quota unit is not given back: the model may have been paid.
			if ok, _ := e.store.TakeQuota(t.Context(), "model_calls:2026-10-04", 1); ok {
				t.Fatal("a failed model call must still use its unit of the daily quota")
			}
		})
	}

	t.Run("suggestions refused", func(t *testing.T) {
		e := newEnv(t, withModel(stubModel{run: run, err: ai.ErrRefused}))
		shipID := e.createShipment("JOB-023")
		doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)
		e.submitManual(doc.ID, invoiceFixture())

		r := e.mustDo("POST", "/v1/shipments/"+shipID+"/suggestions", nil, http.StatusUnprocessableEntity)
		if r.errCode(t) != "model_refused" {
			t.Fatalf("got %s", r.raw)
		}
		events := e.audit(shipID)
		last := events[len(events)-1]
		if last.Action != "suggestions.model_failed" || last.Detail["model"] != "stub-model" || last.Detail["attempts"] != "3" {
			t.Fatalf("last audit = %+v", last)
		}
	})
}

func TestExtractNeedsCheckedFile(t *testing.T) {
	e := newEnv(t, withModel(newFakeModel()))
	shipID := e.createShipment("JOB-024")

	pending := e.startUpload(shipID, domain.DocCommercialInvoice, "application/pdf", int64(len(fakePDF)), fakePDF)
	e.mustDo("POST", "/v1/documents/"+pending.ID+"/extract", nil, http.StatusConflict)

	// The upload URL is still valid for a few minutes after completion, so
	// the file could be swapped. The model must never see an unchecked file.
	doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)
	swapped := append(append([]byte{}, fakePDF[:len(fakePDF)-1]...), 'X') // same size, same type, different bytes
	if err := e.blobs.Put(t.Context(), doc.ID, bytes.NewReader(swapped)); err != nil {
		t.Fatal(err)
	}
	r := e.mustDo("POST", "/v1/documents/"+doc.ID+"/extract", nil, http.StatusUnprocessableEntity)
	if r.errCode(t) != "upload_invalid" || !bytes.Contains(r.raw, []byte("file_changed")) {
		t.Fatalf("got %s", r.raw)
	}
	if e.blobs.has(doc.ID) {
		t.Fatal("swapped file must be deleted")
	}
}

// The per-client daily quota lives in the store, so it holds across server
// instances (on Lambda, each concurrent request may get a fresh instance with
// empty in-memory buckets). It is taken before the global quota, so a client
// that has used its share can't eat into everyone else's.
func TestPerIPModelQuotaSharedAcrossInstances(t *testing.T) {
	model := newFakeModel()
	e := newEnv(t, withModel(model), fixedClock(noon), func(c *Config) { c.ModelCallsPerDay = modelCallsPerIPPerDay + 1 })
	shipID := e.createShipment("JOB-026")
	doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)

	// A second instance sharing the same store, like two Lambda instances
	// sharing DynamoDB. Both have their in-memory limiters switched off, so
	// only the store's counters can stop a request.
	other := New(Config{Store: e.store, Blobs: e.blobs, Extractor: model, Classifier: model,
		ModelName: "fake", ModelCallsPerDay: modelCallsPerIPPerDay + 1, Now: func() time.Time { return noon }})
	for _, srv := range []*Server{e.srv, other} {
		srv.apiLimit = newRateLimiter(rate.Inf, 1)
		srv.modelLimit = newRateLimiter(rate.Inf, 1)
	}
	instances := []http.Handler{e.srv.Handler(), other.Handler()}
	extract := func(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
		return serve(h, "POST", "/v1/documents/"+doc.ID+"/extract", remoteAddr)
	}

	const client = "198.51.100.20:1000"
	for i := range modelCallsPerIPPerDay {
		if rec := extract(instances[i%2], client); rec.Code != http.StatusCreated {
			t.Fatalf("call %d: got %d %s", i+1, rec.Code, rec.Body)
		}
	}
	for _, h := range instances {
		rec := extract(h, client)
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"rate_limited"`) {
			t.Fatalf("over the per-client quota: got %d %s", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Retry-After"); got != "43200" { // 12 h until midnight UTC
			t.Fatalf("Retry-After = %q", got)
		}
	}

	// The refused calls didn't use the global quota: one unit is left, for
	// another client. After that the global cap applies to everyone.
	if rec := extract(instances[1], "198.51.100.21:1000"); rec.Code != http.StatusCreated {
		t.Fatalf("another client: got %d %s", rec.Code, rec.Body)
	}
	rec := extract(instances[0], "198.51.100.22:1000")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"model_quota_exhausted"`) {
		t.Fatalf("over the global quota: got %d %s", rec.Code, rec.Body)
	}
}

// slowModel is the fake model, but the server's clock moves on while it
// "thinks", as it would during a real call.
type slowModel struct {
	*recordingModel
	mu  sync.Mutex
	now time.Time
}

func (m *slowModel) clock() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *slowModel) ExtractInvoice(ctx context.Context, f ai.File) (domain.InvoiceExtraction, domain.ModelRun, error) {
	m.mu.Lock()
	m.now = m.now.Add(90 * time.Second)
	m.mu.Unlock()
	return m.recordingModel.ExtractInvoice(ctx, f)
}

// A model reading is timestamped when the model has answered, not when the
// request began, so it sorts after anything saved while the model ran.
func TestModelExtractionTimestamp(t *testing.T) {
	m := &slowModel{recordingModel: newFakeModel(), now: noon}
	e := newEnv(t, withModel(m), func(c *Config) { c.Now = m.clock })
	shipID := e.createShipment("JOB-027")
	doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)

	var ext domain.Extraction
	e.mustDo("POST", "/v1/documents/"+doc.ID+"/extract", nil, http.StatusCreated).decode(t, &ext)
	if want := noon.Add(90 * time.Second); !ext.CreatedAt.Equal(want) {
		t.Fatalf("created_at = %v, want %v (after the model answered)", ext.CreatedAt, want)
	}
}
