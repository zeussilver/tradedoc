package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/blob"
	"github.com/zeussilver/tradedoc/internal/checks"
	"github.com/zeussilver/tradedoc/internal/domain"
	"github.com/zeussilver/tradedoc/internal/store"
)

// ---- test doubles ----

// memBlobs is an in-memory blob.Store. PresignPut returns a fake URL that
// nothing listens on; tests play the browser by calling Put directly.
type memBlobs struct {
	mu      sync.Mutex
	files   map[string][]byte
	lastTTL time.Duration
}

func newMemBlobs() *memBlobs { return &memBlobs{files: map[string][]byte{}} }

func (m *memBlobs) Put(_ context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[key] = data
	return nil
}

func (m *memBlobs) Get(_ context.Context, key string, max int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	if int64(len(data)) > max {
		return nil, blob.ErrTooLarge
	}
	return append([]byte{}, data...), nil
}

func (m *memBlobs) Size(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[key]
	if !ok {
		return 0, blob.ErrNotFound
	}
	return int64(len(data)), nil
}

func (m *memBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, key)
	return nil
}

func (m *memBlobs) PresignPut(_ context.Context, key, contentType string, _ int64, ttl time.Duration) (blob.PresignedUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastTTL = ttl
	return blob.PresignedUpload{
		URL:       "https://uploads.example.test/" + key + "?sig=fake",
		Method:    http.MethodPut,
		Headers:   map[string]string{"Content-Type": contentType},
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

func (m *memBlobs) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[key]
	return ok
}

// recordingModel is the fake model, plus a record of the lines the
// classifier was sent, so tests can check what would leave the system.
type recordingModel struct {
	ai.Fake
	mu    sync.Mutex
	lines []ai.Line
}

func (m *recordingModel) SuggestCodes(ctx context.Context, lines []ai.Line) ([]ai.Candidate, domain.ModelRun, error) {
	m.mu.Lock()
	m.lines = append(m.lines, lines...)
	m.mu.Unlock()
	return m.Fake.SuggestCodes(ctx, lines)
}

// stubModel returns whatever it is told to, including errors.
type stubModel struct {
	inv domain.InvoiceExtraction
	run domain.ModelRun
	err error
}

func (s stubModel) ExtractInvoice(context.Context, ai.File) (domain.InvoiceExtraction, domain.ModelRun, error) {
	return s.inv, s.run, s.err
}

func (s stubModel) ExtractPackingList(context.Context, ai.File) (domain.PackingListExtraction, domain.ModelRun, error) {
	return domain.PackingListExtraction{}, s.run, s.err
}

func (s stubModel) SuggestCodes(context.Context, []ai.Line) ([]ai.Candidate, domain.ModelRun, error) {
	return nil, s.run, s.err
}

// ---- fixtures: synthetic sample A from docs/CONTRACT.md ----

// fakePDF starts with the PDF magic bytes, which is all DetectContentType checks.
var fakePDF = []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")

// fakePNG is just the PNG signature plus a few bytes.
var fakePNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func text(v, quote string) domain.Field[string] { return domain.Of(v, domain.ConfidenceHigh, 1, quote) }
func num(v int64, quote string) domain.Field[int64] {
	return domain.Of(v, domain.ConfidenceHigh, 1, quote)
}

// Quantities are decimal strings as printed, without thousands separators:
// "1,200 PCS" on the page is stored as "1200".
func invoiceFixture() domain.InvoiceExtraction {
	line := func(no int, desc, qty, price string, total int64) domain.InvoiceLine {
		return domain.InvoiceLine{
			LineNo: no, Description: text(desc, desc), Quantity: text(qty, qty+" PCS"), Unit: text("PCS", "PCS"),
			UnitPrice: text(price, price), LineTotalMinor: num(total, "total"), OriginCountry: text("CN", "Country of origin: China"),
		}
	}
	return domain.InvoiceExtraction{
		InvoiceNumber: text("INV-2026-0042", "Invoice No: INV-2026-0042"),
		InvoiceDate:   text("2026-09-15", "15 Sep 2026"),
		SellerName:    text("Harbour Ceramics Co., Ltd.", "Harbour Ceramics Co., Ltd."),
		BuyerName:     text("Southbank Homewares Pty Ltd", "Southbank Homewares Pty Ltd"),
		Currency:      text("USD", "USD"),
		Incoterm:      text("FOB", "FOB Ningbo"),
		LineItems: []domain.InvoiceLine{
			line(1, "Porcelain dinner plate, 27 cm, white glazed", "1200", "3.50", 420000),
			line(2, "Stoneware coffee mug, 350 ml", "800", "2.10", 168000),
			line(3, "Bamboo serving board, 40 x 25 cm", "300", "4.80", 144000),
		},
		TotalMinor: num(730200, "USD 7,302.00"),
	}
}

func packingFixture() domain.PackingListExtraction {
	line := func(no int, desc, qty string, pkgs, grams int64) domain.PackingLine {
		return domain.PackingLine{
			LineNo: no, Description: text(desc, desc), Quantity: text(qty, qty+" PCS"), Unit: text("PCS", "PCS"),
			Packages: num(pkgs, "CTNS"), GrossWeightGrams: num(grams, "kg"),
		}
	}
	return domain.PackingListExtraction{
		PackingListNumber: text("PL-2026-0042", "PL-2026-0042"),
		InvoiceRef:        text("INV-2026-0042", "INV-2026-0042"),
		LineItems: []domain.PackingLine{
			line(1, "Porcelain dinner plate, 27 cm", "1200", 50, 540000),
			line(2, "Ceramic coffee mug, 350 ml", "780", 20, 210000),
			line(3, "Bamboo serving board, 40 x 25 cm", "300", 12, 186000),
		},
		TotalPackages:         num(84, "84 CTNS"),
		TotalGrossWeightGrams: num(936000, "936.00 kg"),
	}
}

// Illustrative test data only: the codes are not a classification.
func candidatesFixture() []ai.Candidate {
	c := func(line, rank int, code string) ai.Candidate {
		return ai.Candidate{LineNo: line, Rank: rank, HSCode: code, Rationale: "test rationale citing the heading text (GIR 1)",
			Confidence: domain.ConfidenceMedium, MissingInfo: []string{"material composition"}}
	}
	return []ai.Candidate{
		c(1, 1, "691110"),
		c(1, 2, "691200"),
		c(2, 1, "691200"),
		c(3, 1, "441911"),
		c(9, 1, "691110"), // line 9 isn't on the invoice: must be dropped
		c(2, 2, "69AB"),   // fails Validate: must be dropped
	}
}

func newFakeModel() *recordingModel {
	inv, pl := invoiceFixture(), packingFixture()
	return &recordingModel{Fake: ai.Fake{Invoice: &inv, PackingList: &pl, Candidates: candidatesFixture()}}
}

// ---- test server and request helpers ----

type testEnv struct {
	t     *testing.T
	srv   *Server
	url   string
	store *store.Memory
	blobs *memBlobs
}

func newEnv(t *testing.T, configure ...func(*Config)) *testEnv {
	t.Helper()
	st, bl := store.NewMemory(), newMemBlobs()
	cfg := Config{Store: st, Blobs: bl, ModelName: "fake"}
	for _, f := range configure {
		f(&cfg)
	}
	srv := New(cfg)
	// Most tests make many quick requests from one address. The limiters
	// have their own tests, so switch them off here.
	srv.apiLimit = newRateLimiter(rate.Inf, 1)
	srv.modelLimit = newRateLimiter(rate.Inf, 1)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, srv: srv, url: ts.URL, store: st, blobs: bl}
}

// withLocalUploads mounts a stand-in for blob.Local's upload handler, as in
// local development. The multipart upload route only exists then.
func withLocalUploads(c *Config) {
	c.LocalUploads = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

// fixedClock makes the server's clock stop at t, so dates and Retry-After
// values are predictable.
func fixedClock(t time.Time) func(*Config) {
	return func(c *Config) { c.Now = func() time.Time { return t } }
}

// noon is a fixed time 12 hours before midnight UTC.
var noon = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func withModel(m interface {
	ai.Extractor
	ai.Classifier
}) func(*Config) {
	return func(c *Config) { c.Extractor, c.Classifier = m, m }
}

type response struct {
	code   int
	header http.Header
	raw    []byte
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.raw, v); err != nil {
		t.Fatalf("decode %s: %v", r.raw, err)
	}
}

func (r response) errCode(t *testing.T) string {
	t.Helper()
	var body struct{ Error apiError }
	r.decode(t, &body)
	return body.Error.Code
}

// do sends a request. body may be nil, a raw JSON string, or any value to
// marshal as JSON.
func (e *testEnv) do(method, path string, body any) response {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.url+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return e.send(req)
}

func (e *testEnv) send(req *http.Request) response {
	e.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return response{code: resp.StatusCode, header: resp.Header, raw: raw}
}

func (e *testEnv) mustDo(method, path string, body any, want int) response {
	e.t.Helper()
	r := e.do(method, path, body)
	if r.code != want {
		e.t.Fatalf("%s %s: want %d, got %d %s", method, path, want, r.code, r.raw)
	}
	return r
}

func (e *testEnv) createShipment(ref string) string {
	e.t.Helper()
	var sh domain.Shipment
	e.mustDo("POST", "/v1/shipments", map[string]string{"reference": ref}, http.StatusCreated).decode(e.t, &sh)
	return sh.ID
}

type startUploadBody struct {
	Document domain.Document      `json:"document"`
	Upload   blob.PresignedUpload `json:"upload"`
}

// startUpload asks for an upload URL and "PUTs" data to it the way a browser
// would, but straight into the fake store. It returns the pending document.
func (e *testEnv) startUpload(shipID string, t domain.DocType, contentType string, declared int64, data []byte) domain.Document {
	e.t.Helper()
	var got startUploadBody
	e.mustDo("POST", "/v1/shipments/"+shipID+"/uploads", map[string]any{
		"type": t, "filename": "file.bin", "content_type": contentType, "size_bytes": declared,
	}, http.StatusCreated).decode(e.t, &got)
	if data != nil {
		u, err := url.Parse(got.Upload.URL)
		if err != nil {
			e.t.Fatal(err)
		}
		key := strings.TrimPrefix(u.Path, "/")
		if err := e.blobs.Put(context.Background(), key, bytes.NewReader(data)); err != nil {
			e.t.Fatal(err)
		}
	}
	return got.Document
}

// upload runs the whole direct-upload flow and returns the uploaded document.
func (e *testEnv) upload(shipID string, t domain.DocType, data []byte) domain.Document {
	e.t.Helper()
	doc := e.startUpload(shipID, t, http.DetectContentType(data), int64(len(data)), data)
	var done domain.Document
	e.mustDo("POST", "/v1/documents/"+doc.ID+"/complete", nil, http.StatusOK).decode(e.t, &done)
	return done
}

func (e *testEnv) submitManual(docID string, payload any) domain.Extraction {
	e.t.Helper()
	var ext domain.Extraction
	e.mustDo("POST", "/v1/documents/"+docID+"/extractions", map[string]any{"source": "manual", "payload": payload},
		http.StatusCreated).decode(e.t, &ext)
	return ext
}

func (e *testEnv) audit(shipID string) []domain.AuditEvent {
	e.t.Helper()
	var body struct{ Events []domain.AuditEvent }
	e.mustDo("GET", "/v1/shipments/"+shipID+"/audit", nil, http.StatusOK).decode(e.t, &body)
	return body.Events
}

func actions(events []domain.AuditEvent) string {
	var out []string
	for _, ev := range events {
		out = append(out, ev.Action)
	}
	return strings.Join(out, ",")
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---- the whole workflow ----

func TestHappyPath(t *testing.T) {
	model := newFakeModel()
	e := newEnv(t, withModel(model))
	shipID := e.createShipment("JOB-001")

	// 1. Ask for an upload URL: the document starts pending, the URL is short-lived.
	pending := e.startUpload(shipID, domain.DocCommercialInvoice, "application/pdf", int64(len(fakePDF)), fakePDF)
	if pending.Status != domain.StatusPendingUpload {
		t.Fatalf("status = %s, want pending_upload", pending.Status)
	}
	if e.blobs.lastTTL != 5*time.Minute {
		t.Fatalf("upload URL TTL = %v, want 5m", e.blobs.lastTTL)
	}

	// 2. Complete: the server checks the stored bytes and records their hash.
	var invDoc domain.Document
	e.mustDo("POST", "/v1/documents/"+pending.ID+"/complete", nil, http.StatusOK).decode(t, &invDoc)
	if invDoc.Status != domain.StatusUploaded || invDoc.SHA256 != sha(fakePDF) {
		t.Fatalf("completed document = %+v", invDoc)
	}
	plDoc := e.upload(shipID, domain.DocPackingList, fakePNG)
	if plDoc.ContentType != "image/png" {
		t.Fatalf("packing list content type = %s", plDoc.ContentType)
	}

	// 3. The model reads both documents.
	var invExt, plExt domain.Extraction
	e.mustDo("POST", "/v1/documents/"+invDoc.ID+"/extract", nil, http.StatusCreated).decode(t, &invExt)
	e.mustDo("POST", "/v1/documents/"+plDoc.ID+"/extract", nil, http.StatusCreated).decode(t, &plExt)
	if invExt.Source != "model:fake" || invExt.Run == nil || invExt.Run.Model != "fake" || invExt.Invoice == nil {
		t.Fatalf("invoice extraction = %+v", invExt)
	}
	if plExt.PackingList == nil {
		t.Fatal("packing list extraction has no packing list")
	}
	var docs struct{ Documents []domain.Document }
	e.mustDo("GET", "/v1/shipments/"+shipID+"/documents", nil, http.StatusOK).decode(t, &docs)
	for _, d := range docs.Documents {
		if d.Status != domain.StatusExtracted {
			t.Fatalf("document %s status = %s, want extracted", d.ID, d.Status)
		}
	}

	// 4. Cross-document checks use exactly those two readings.
	var run domain.CheckRun
	r := e.mustDo("POST", "/v1/shipments/"+shipID+"/checks", nil, http.StatusCreated)
	r.decode(t, &run)
	if run.InvoiceExtractionID != invExt.ID || run.PackingListExtractionID != plExt.ID {
		t.Fatalf("check run used %s/%s, want %s/%s", run.InvoiceExtractionID, run.PackingListExtractionID, invExt.ID, plExt.ID)
	}
	if !bytes.Contains(r.raw, []byte(`"findings":[`)) {
		t.Fatalf("findings must be a JSON list, got %s", r.raw)
	}

	// 5. Candidate codes: unknown lines and invalid candidates are dropped.
	var sugs struct {
		Suggestions []domain.ClassificationSuggestion
	}
	e.mustDo("POST", "/v1/shipments/"+shipID+"/suggestions", nil, http.StatusCreated).decode(t, &sugs)
	if len(sugs.Suggestions) != 4 {
		t.Fatalf("got %d suggestions, want 4", len(sugs.Suggestions))
	}
	inv := invoiceFixture()
	for _, sg := range sugs.Suggestions {
		line := inv.LineItems[sg.LineNo-1]
		if sg.Status != domain.SuggestionPending || sg.Source != "model:fake" ||
			sg.DocumentID != invDoc.ID || sg.ExtractionID != invExt.ID || sg.ShipmentID != shipID ||
			sg.Description != *line.Description.Value || sg.Evidence == nil || sg.Evidence.Quote != line.Description.Evidence.Quote {
			t.Fatalf("suggestion = %+v", sg)
		}
	}
	// Only description and unit were sent to the classifier: no prices,
	// quantities, parties or origin.
	want := []ai.Line{
		{LineNo: 1, Description: "Porcelain dinner plate, 27 cm, white glazed", Unit: "PCS"},
		{LineNo: 2, Description: "Stoneware coffee mug, 350 ml", Unit: "PCS"},
		{LineNo: 3, Description: "Bamboo serving board, 40 x 25 cm", Unit: "PCS"},
	}
	model.mu.Lock()
	sent := model.lines
	model.mu.Unlock()
	if len(sent) != len(want) {
		t.Fatalf("classifier got %+v", sent)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("classifier line %d = %+v, want %+v", i, sent[i], want[i])
		}
	}

	// 6. A broker reviews every candidate: accept rank 1, reject the rest.
	for _, sg := range sugs.Suggestions {
		decision := "rejected"
		if sg.Rank == 1 {
			decision = "accepted"
		}
		var got domain.ClassificationSuggestion
		e.mustDo("POST", "/v1/suggestions/"+sg.ID+"/review",
			map[string]string{"decision": decision, "reviewer": "Alex Broker", "note": "checked against the tariff"},
			http.StatusOK).decode(t, &got)
		if string(got.Status) != decision || got.ReviewedBy != "Alex Broker" || got.ReviewedAt == nil {
			t.Fatalf("reviewed suggestion = %+v", got)
		}
	}

	// 7. Sign-off on the latest run. A note is given so this passes whatever
	// the checks find in sample A's deliberate mistakes.
	errorsInRun := 0
	for _, f := range run.Findings {
		if f.Severity == domain.SeverityError {
			errorsInRun++
		}
	}
	var so domain.SignOff
	e.mustDo("POST", "/v1/shipments/"+shipID+"/signoffs", map[string]string{
		"decision": "approved", "reviewer": "Alex Broker", "check_run_id": run.ID, "note": "supplier confirmed the corrections",
	}, http.StatusCreated).decode(t, &so)
	if so.OpenErrors != errorsInRun || so.CheckRunID != run.ID {
		t.Fatalf("sign-off = %+v", so)
	}
	var list struct{ Signoffs []domain.SignOff }
	e.mustDo("GET", "/v1/shipments/"+shipID+"/signoffs", nil, http.StatusOK).decode(t, &list)
	if len(list.Signoffs) != 1 {
		t.Fatalf("signoffs = %+v", list.Signoffs)
	}

	// 8. The audit trail shows every step, in order, with the right actor.
	events := e.audit(shipID)
	wantActions := "shipment.created," +
		"document.upload_started,document.uploaded,document.upload_started,document.uploaded," +
		"extraction.model_completed,extraction.model_completed,checks.completed,suggestions.created," +
		"suggestion.accepted,suggestion.rejected,suggestion.accepted,suggestion.accepted," +
		"shipment.signed_off"
	if got := actions(events); got != wantActions {
		t.Fatalf("audit =\n%s\nwant\n%s", got, wantActions)
	}
	for _, ev := range events {
		wantActor := "demo-user"
		if strings.HasPrefix(ev.Action, "suggestion.") || ev.Action == "shipment.signed_off" {
			wantActor = "Alex Broker"
		}
		if ev.Actor != wantActor {
			t.Fatalf("%s actor = %q, want %q", ev.Action, ev.Actor, wantActor)
		}
		if ev.Action == "extraction.model_completed" {
			if len(ev.Detail) != 4 || ev.Detail["model"] != "fake" || ev.Detail["attempts"] != "1" ||
				ev.Detail["input_tokens"] == "" || ev.Detail["output_tokens"] == "" {
				t.Fatalf("model audit detail = %v", ev.Detail)
			}
		}
		if ev.Action == "suggestion.accepted" && ev.Detail["note"] != "" {
			t.Fatal("review notes must not be copied into the audit log")
		}
	}
	if d := events[len(events)-1].Detail; d["decision"] != "approved" || d["check_run_id"] != run.ID {
		t.Fatalf("sign-off audit detail = %v", d)
	}
}

// ---- earlier behaviour that must keep working ----

func uploadMultipart(t *testing.T, e *testEnv, shipID, docType, filename string, data []byte) response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("type", docType)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.url+"/v1/shipments/"+shipID+"/documents", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return e.send(req)
}

func TestManualExtractionFlow(t *testing.T) {
	e := newEnv(t, withLocalUploads)
	shipID := e.createShipment("JOB-001")

	// Multipart upload: the file passes through the server.
	r := uploadMultipart(t, e, shipID, "commercial_invoice", "invoice.pdf", fakePDF)
	if r.code != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.code, r.raw)
	}
	var doc domain.Document
	r.decode(t, &doc)
	if bytes.Contains(r.raw, []byte("StorageKey")) || bytes.Contains(r.raw, []byte("storage_key")) {
		t.Fatal("storage key must not be returned to clients")
	}
	if doc.Status != domain.StatusUploaded || !e.blobs.has(doc.ID) {
		t.Fatalf("document = %+v", doc)
	}

	// An invalid extraction is rejected with every problem listed.
	bad := e.mustDo("POST", "/v1/documents/"+doc.ID+"/extractions",
		`{"payload":{"invoice_number":{"value":"INV-1"},"currency":{"value":"usd","confidence":"high","evidence":{"page":1,"quote":"usd"}}}}`,
		http.StatusUnprocessableEntity)
	var body struct {
		Error struct{ Details []domain.FieldError }
	}
	bad.decode(t, &body)
	if len(body.Error.Details) < 3 {
		t.Fatalf("expected several validation errors, got %v", body.Error.Details)
	}

	// Clients can't pass their own data off as model output.
	e.mustDo("POST", "/v1/documents/"+doc.ID+"/extractions",
		map[string]any{"source": "model:claude-opus-5-5", "payload": invoiceFixture()}, http.StatusUnprocessableEntity)

	// A valid one is accepted and marks the document extracted.
	ext := e.submitManual(doc.ID, invoiceFixture())
	if ext.Source != "manual" || ext.Run != nil {
		t.Fatalf("manual extraction = %+v", ext)
	}
	var docs struct{ Documents []domain.Document }
	e.mustDo("GET", "/v1/shipments/"+shipID+"/documents", nil, http.StatusOK).decode(t, &docs)
	if docs.Documents[0].Status != domain.StatusExtracted {
		t.Fatalf("status = %s, want extracted", docs.Documents[0].Status)
	}

	want := "shipment.created,document.uploaded,extraction.rejected,extraction.accepted"
	if got := actions(e.audit(shipID)); got != want {
		t.Fatalf("audit = %s, want %s", got, want)
	}
}

func TestMultipartUploadRejectsNonPDF(t *testing.T) {
	e := newEnv(t, withLocalUploads)
	shipID := e.createShipment("JOB-002")
	// An HTML file renamed to .pdf is still HTML: we sniff content, not the extension.
	r := uploadMultipart(t, e, shipID, "commercial_invoice", "invoice.pdf", []byte("<html><script>alert(1)</script></html>"))
	if r.code != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d", r.code)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	e := newEnv(t)
	r := e.do("POST", "/v1/shipments", `{"refernce":"typo"}`)
	if r.code != http.StatusBadRequest || r.errCode(t) != "invalid_json" {
		t.Fatalf("want 400 invalid_json for unknown field, got %d %s", r.code, r.raw)
	}
}

func TestNotFound(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/v1/shipments/shp_missing", nil)
	if r.code != http.StatusNotFound || r.errCode(t) != "not_found" {
		t.Fatalf("want 404 with error body, got %d %s", r.code, r.raw)
	}
}

func TestConfig(t *testing.T) {
	var on, off configResponse
	newEnv(t, withModel(newFakeModel())).mustDo("GET", "/v1/config", nil, http.StatusOK).decode(t, &on)
	newEnv(t).mustDo("GET", "/v1/config", nil, http.StatusOK).decode(t, &off)
	if !on.ModelEnabled || on.Model != "fake" || off.ModelEnabled {
		t.Fatalf("config on=%+v off=%+v", on, off)
	}
	if on.MaxUploadBytes != 10485760 || on.MaxImageBytes != 5242880 ||
		strings.Join(on.AllowedTypes, ",") != "application/pdf,image/png,image/jpeg" {
		t.Fatalf("config = %+v", on)
	}
	// The UI formats money from the same minor-unit table as the checks.
	if jpy, ok := on.CurrencyMinorUnits["JPY"]; !ok || jpy != 0 ||
		!maps.Equal(on.CurrencyMinorUnits, checks.MinorUnitExceptions()) {
		t.Fatalf("currency_minor_units = %v", on.CurrencyMinorUnits)
	}
}

// The multipart upload only exists in local development: a Lambda Function
// URL can't carry a 10 MiB request body.
func TestMultipartUploadOnlyLocal(t *testing.T) {
	e := newEnv(t) // no LocalUploads, as on AWS
	shipID := e.createShipment("JOB-003")
	r := uploadMultipart(t, e, shipID, "commercial_invoice", "invoice.pdf", fakePDF)
	if r.code != http.StatusNotFound || r.errCode(t) != "not_found" {
		t.Fatalf("want 404 not_found, got %d %s", r.code, r.raw)
	}
	if docs, _ := e.store.ListDocuments(t.Context(), shipID); len(docs) != 0 {
		t.Fatalf("documents = %+v", docs)
	}
}

func TestCreateShipmentReference(t *testing.T) {
	tests := []struct {
		name      string
		reference string
		want      int
	}{
		{"short", "JOB-001", http.StatusCreated},
		{"64 characters", strings.Repeat("a", 64), http.StatusCreated},
		// Characters, not bytes: 64 Chinese characters are 192 bytes.
		{"64 Chinese characters", strings.Repeat("货", 64), http.StatusCreated},
		{"65 characters", strings.Repeat("a", 65), http.StatusUnprocessableEntity},
		{"65 Chinese characters", strings.Repeat("货", 65), http.StatusUnprocessableEntity},
		{"blank", "   ", http.StatusUnprocessableEntity},
		{"newline", "JOB-1\nadmin", http.StatusUnprocessableEntity},
		{"escape code", "JOB-1\x1b[2J", http.StatusUnprocessableEntity},
	}
	e := newEnv(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.mustDo("POST", "/v1/shipments", map[string]string{"reference": tt.reference}, tt.want)
			if tt.want != http.StatusCreated && r.errCode(t) != "validation_failed" {
				t.Fatalf("got %s", r.raw)
			}
		})
	}
}

// Manual extraction bodies are capped at 256 KiB, so a stored reading stays
// inside DynamoDB's 400 KB item limit.
func TestManualExtractionBodyLimit(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-004")
	doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)

	// body builds a request of exactly n bytes. The padding sits inside the
	// payload, so the whole body has to be read to decode it.
	body := func(n int) string {
		const prefix, suffix = `{"source":"manual","payload":{"pad":"`, `"}}`
		return prefix + strings.Repeat("x", n-len(prefix)-len(suffix)) + suffix
	}
	tests := []struct {
		name string
		size int
		want int
		code string
	}{
		// At the limit the body is read, then refused for what it says.
		{"at 256 KiB", 256 << 10, http.StatusBadRequest, "invalid_payload"},
		{"one byte over", 256<<10 + 1, http.StatusRequestEntityTooLarge, "body_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.mustDo("POST", "/v1/documents/"+doc.ID+"/extractions", body(tt.size), tt.want)
			if got := r.errCode(t); got != tt.code {
				t.Fatalf("code = %s, want %s", got, tt.code)
			}
		})
	}
}

// tooLargeStore behaves like DynamoDB refusing an item over 400 KB, for the
// records that can grow with the documents.
type tooLargeStore struct{ *store.Memory }

func (tooLargeStore) AddExtraction(context.Context, domain.Extraction) error {
	return store.ErrTooLarge
}

func (tooLargeStore) AddCheckRun(context.Context, domain.CheckRun) error {
	return store.ErrTooLarge
}

func TestStoreTooLarge(t *testing.T) {
	e := newEnv(t, withModel(newFakeModel()), func(c *Config) { c.Store = tooLargeStore{c.Store.(*store.Memory)} })
	shipID := e.createShipment("JOB-005")
	inv := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)

	tooLarge := func(path string, body any) {
		t.Helper()
		if r := e.mustDo("POST", path, body, http.StatusRequestEntityTooLarge); r.errCode(t) != "too_large" {
			t.Fatalf("%s: got %s", path, r.raw)
		}
	}
	tooLarge("/v1/documents/"+inv.ID+"/extractions", map[string]any{"payload": invoiceFixture()}) // typed in
	tooLarge("/v1/documents/"+inv.ID+"/extract", nil)                                             // from the model
	if got, _ := e.store.GetDocument(t.Context(), inv.ID); got.Status != domain.StatusUploaded {
		t.Fatalf("status = %s: a reading that wasn't stored must not mark the document read", got.Status)
	}

	// A check run built from two big readings can be too large as well. The
	// readings are put straight into the underlying store.
	pl := e.upload(shipID, domain.DocPackingList, fakePDF)
	invFix, plFix := invoiceFixture(), packingFixture()
	for _, x := range []struct {
		doc domain.Document
		ext domain.Extraction
	}{
		{inv, domain.Extraction{ID: newID("ext"), DocumentID: inv.ID, Source: "manual", Invoice: &invFix}},
		{pl, domain.Extraction{ID: newID("ext"), DocumentID: pl.ID, Source: "manual", PackingList: &plFix}},
	} {
		if err := e.store.AddExtraction(t.Context(), x.ext); err != nil {
			t.Fatal(err)
		}
		x.doc.Status = domain.StatusExtracted
		if err := e.store.UpdateDocument(t.Context(), x.doc); err != nil {
			t.Fatal(err)
		}
	}
	if r := e.mustDo("POST", "/v1/shipments/"+shipID+"/checks", nil, http.StatusRequestEntityTooLarge); r.errCode(t) != "too_large" {
		t.Fatalf("checks: got %s", r.raw)
	}
}
