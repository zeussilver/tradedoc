package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zeussilver/tradedoc/internal/blob"
	"github.com/zeussilver/tradedoc/internal/store"
)

// fakePDF starts with the PDF magic bytes, which is all DetectContentType checks.
var fakePDF = []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := New(store.NewMemory(), blob.Local{Dir: t.TempDir()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return send(t, req)
}

func upload(t *testing.T, url, docType, filename string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("type", docType)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return send(t, req)
}

func send(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestEndToEnd(t *testing.T) {
	ts := newTestServer(t)

	// 1. create a shipment
	code, sh := doJSON(t, "POST", ts.URL+"/v1/shipments", `{"reference":"JOB-001"}`)
	if code != http.StatusCreated {
		t.Fatalf("create shipment: %d %v", code, sh)
	}
	shipID := sh["id"].(string)

	// 2. upload an invoice
	code, doc := upload(t, ts.URL+"/v1/shipments/"+shipID+"/documents", "commercial_invoice", "invoice.pdf", fakePDF)
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %v", code, doc)
	}
	docID := doc["id"].(string)
	if _, leaked := doc["StorageKey"]; leaked {
		t.Fatal("storage key must not be returned to clients")
	}

	// 3. an invalid extraction is rejected with every problem listed
	code, bad := doJSON(t, "POST", ts.URL+"/v1/documents/"+docID+"/extractions",
		`{"payload":{"invoice_number":{"value":"INV-1"},"currency":{"value":"usd","confidence":"high","evidence":{"page":1,"quote":"usd"}}}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("bad extraction: want 422, got %d %v", code, bad)
	}
	details := bad["error"].(map[string]any)["details"].([]any)
	if len(details) < 3 {
		t.Fatalf("expected several validation errors, got %v", details)
	}

	// 4. the sample extraction is accepted
	sample, err := os.ReadFile("../../testdata/invoice_extraction.json")
	if err != nil {
		t.Fatal(err)
	}
	code, ext := doJSON(t, "POST", ts.URL+"/v1/documents/"+docID+"/extractions", string(sample))
	if code != http.StatusCreated {
		t.Fatalf("good extraction: %d %v", code, ext)
	}

	// 5. the audit trail shows each step, in order
	_, audit := doJSON(t, "GET", ts.URL+"/v1/shipments/"+shipID+"/audit", "")
	var actions []string
	for _, e := range audit["events"].([]any) {
		actions = append(actions, e.(map[string]any)["action"].(string))
	}
	want := "shipment.created,document.uploaded,extraction.rejected,extraction.accepted"
	if got := strings.Join(actions, ","); got != want {
		t.Fatalf("audit = %s, want %s", got, want)
	}
}

func TestUploadRejectsNonPDF(t *testing.T) {
	ts := newTestServer(t)
	_, sh := doJSON(t, "POST", ts.URL+"/v1/shipments", `{"reference":"JOB-002"}`)
	// An HTML file renamed to .pdf is still HTML: we sniff content, not the extension.
	code, _ := upload(t, ts.URL+"/v1/shipments/"+sh["id"].(string)+"/documents",
		"commercial_invoice", "invoice.pdf", []byte("<html><script>alert(1)</script></html>"))
	if code != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d", code)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	ts := newTestServer(t)
	code, _ := doJSON(t, "POST", ts.URL+"/v1/shipments", `{"refernce":"typo"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for unknown field, got %d", code)
	}
}

func TestNotFound(t *testing.T) {
	ts := newTestServer(t)
	code, body := doJSON(t, "GET", ts.URL+"/v1/shipments/shp_missing", "")
	if code != http.StatusNotFound || body["error"] == nil {
		t.Fatalf("want 404 with error body, got %d %v", code, body)
	}
}
