package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zeussilver/tradedoc/internal/domain"
)

func TestStartUploadValidation(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-010")
	body := func(contentType string, size int64, filename string) map[string]any {
		return map[string]any{"type": "commercial_invoice", "filename": filename, "content_type": contentType, "size_bytes": size}
	}
	tests := []struct {
		name     string
		body     any
		want     int
		badField string // for 422: the field the error must name
	}{
		{"pdf at the 10 MiB limit", body("application/pdf", 10<<20, "inv.pdf"), http.StatusCreated, ""},
		{"pdf over 10 MiB", body("application/pdf", 10<<20+1, "inv.pdf"), http.StatusUnprocessableEntity, "size_bytes"},
		{"png at the 5 MiB limit", body("image/png", 5<<20, "scan.png"), http.StatusCreated, ""},
		{"png over 5 MiB", body("image/png", 5<<20+1, "scan.png"), http.StatusUnprocessableEntity, "size_bytes"},
		{"jpeg over 5 MiB", body("image/jpeg", 6<<20, "scan.jpg"), http.StatusUnprocessableEntity, "size_bytes"},
		{"empty file", body("application/pdf", 0, "inv.pdf"), http.StatusUnprocessableEntity, "size_bytes"},
		{"html", body("text/html", 100, "inv.pdf"), http.StatusUnprocessableEntity, "content_type"},
		{"bad document type", map[string]any{"type": "bill_of_lading", "filename": "a.pdf", "content_type": "application/pdf", "size_bytes": 10},
			http.StatusUnprocessableEntity, "type"},
		{"filename too long", body("application/pdf", 10, strings.Repeat("a", 197)+".pdf"), http.StatusUnprocessableEntity, "filename"},
		{"filename missing", body("application/pdf", 10, "  "), http.StatusUnprocessableEntity, "filename"},
		{"control characters in filename", body("application/pdf", 10, "inv\n.pdf"), http.StatusUnprocessableEntity, "filename"},
		{"unknown field", `{"type":"commercial_invoice","filename":"a.pdf","content_type":"application/pdf","size_bytes":10,"x":1}`,
			http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.do("POST", "/v1/shipments/"+shipID+"/uploads", tt.body)
			if r.code != tt.want {
				t.Fatalf("want %d, got %d %s", tt.want, r.code, r.raw)
			}
			if tt.badField != "" {
				var body struct {
					Error struct{ Details []domain.FieldError }
				}
				r.decode(t, &body)
				if len(body.Error.Details) != 1 || body.Error.Details[0].Path != tt.badField {
					t.Fatalf("details = %+v, want one error on %s", body.Error.Details, tt.badField)
				}
			}
		})
	}

	t.Run("path is stripped from the filename", func(t *testing.T) {
		var got startUploadBody
		e.mustDo("POST", "/v1/shipments/"+shipID+"/uploads", body("application/pdf", 10, "../../etc/invoice.pdf"), http.StatusCreated).decode(t, &got)
		if got.Document.Filename != "invoice.pdf" {
			t.Fatalf("filename = %q", got.Document.Filename)
		}
	})
	t.Run("unknown shipment", func(t *testing.T) {
		e.mustDo("POST", "/v1/shipments/shp_missing/uploads", body("application/pdf", 10, "a.pdf"), http.StatusNotFound)
	})
}

func TestStartUploadDoesNotLeakStorageKey(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-011")
	r := e.mustDo("POST", "/v1/shipments/"+shipID+"/uploads", map[string]any{
		"type": "commercial_invoice", "filename": "a.pdf", "content_type": "application/pdf", "size_bytes": 10,
	}, http.StatusCreated)
	var raw struct{ Document map[string]any }
	r.decode(t, &raw)
	for k := range raw.Document {
		if strings.Contains(strings.ToLower(k), "storage") {
			t.Fatalf("document exposes %q", k)
		}
	}
}

func TestCompleteRejectsBadUploads(t *testing.T) {
	html := []byte("<html><script>alert(1)</script></html>")
	tests := []struct {
		name        string
		contentType string
		declared    int64
		data        []byte // nil: nothing is uploaded
		reason      string
	}{
		{"size differs from declared", "application/pdf", int64(len(fakePDF)) + 10, fakePDF, "size_mismatch"},
		{"html declared as pdf", "application/pdf", int64(len(html)), html, "type_mismatch"},
		{"pdf declared as png", "image/png", int64(len(fakePDF)), fakePDF, "type_mismatch"},
		{"nothing uploaded", "application/pdf", 100, nil, "file_missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			shipID := e.createShipment("JOB-012")
			doc := e.startUpload(shipID, domain.DocCommercialInvoice, tt.contentType, tt.declared, tt.data)

			r := e.mustDo("POST", "/v1/documents/"+doc.ID+"/complete", nil, http.StatusUnprocessableEntity)
			var body struct{ Error apiError }
			r.decode(t, &body)
			if body.Error.Code != "upload_invalid" || !strings.Contains(string(r.raw), tt.reason) {
				t.Fatalf("got %s, want upload_invalid %s", r.raw, tt.reason)
			}
			if e.blobs.has(doc.ID) {
				t.Fatal("rejected file must be deleted from storage")
			}
			got, err := e.store.GetDocument(t.Context(), doc.ID)
			if err != nil || got.Status != domain.StatusRejected {
				t.Fatalf("document status = %s (%v), want rejected", got.Status, err)
			}
			events := e.audit(shipID)
			last := events[len(events)-1]
			if last.Action != "document.rejected" || last.Detail["reason"] != tt.reason {
				t.Fatalf("last audit = %+v", last)
			}
			// A rejected document can't be completed again or read.
			e.mustDo("POST", "/v1/documents/"+doc.ID+"/complete", nil, http.StatusConflict)
			e.mustDo("POST", "/v1/documents/"+doc.ID+"/extractions", map[string]any{"payload": invoiceFixture()}, http.StatusConflict)
		})
	}
}

func TestCompleteTwiceConflicts(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-013")
	doc := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)
	r := e.mustDo("POST", "/v1/documents/"+doc.ID+"/complete", nil, http.StatusConflict)
	if r.errCode(t) != "conflict" {
		t.Fatalf("got %s", r.raw)
	}
}

func TestMultipartRejectsLargeImage(t *testing.T) {
	e := newEnv(t, withLocalUploads)
	shipID := e.createShipment("JOB-014")
	big := append(append([]byte{}, fakePNG...), make([]byte, 5<<20)...)
	r := uploadMultipart(t, e, shipID, "commercial_invoice", "scan.png", big)
	if r.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 for a PNG over 5 MiB, got %d %s", r.code, r.raw)
	}
}
