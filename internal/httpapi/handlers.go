package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zeussilver/tradedoc/internal/domain"
	"github.com/zeussilver/tradedoc/internal/store"
)

const (
	maxUploadBytes = 10 << 20    // 10 MiB
	actor          = "demo-user" // replaced by real auth later
)

// Allowed upload types, decided by sniffing the file's first bytes rather
// than trusting the client's Content-Type header or file extension.
var allowedContent = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) audit(r *http.Request, shipmentID, action, entityType, entityID string, detail map[string]string) {
	err := s.store.AppendAudit(r.Context(), domain.AuditEvent{
		ID: newID("aud"), At: s.now(), Actor: actor, Action: action,
		EntityType: entityType, EntityID: entityID, ShipmentID: shipmentID, Detail: detail,
	})
	if err != nil {
		slog.Error("append audit", "request_id", requestIDFrom(r.Context()), "err", err)
	}
}

// notFoundOr500 maps store errors to HTTP status codes in one place.
func notFoundOr500(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", what+" not found", nil)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
}

// ---- shipments ----

type createShipmentReq struct {
	Reference string `json:"reference"`
}

func (s *Server) createShipment(w http.ResponseWriter, r *http.Request) {
	var req createShipmentReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Reference = strings.TrimSpace(req.Reference)
	if req.Reference == "" || len(req.Reference) > 64 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request",
			domain.ValidationErrors{{Path: "reference", Message: "must be 1-64 characters"}})
		return
	}
	sh := domain.Shipment{ID: newID("shp"), Reference: req.Reference, CreatedAt: s.now()}
	if err := s.store.CreateShipment(r.Context(), sh); err != nil {
		notFoundOr500(w, err, "shipment")
		return
	}
	s.audit(r, sh.ID, "shipment.created", "shipment", sh.ID, nil)
	writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) getShipment(w http.ResponseWriter, r *http.Request) {
	sh, err := s.store.GetShipment(r.Context(), r.PathValue("id"))
	if err != nil {
		notFoundOr500(w, err, "shipment")
		return
	}
	writeJSON(w, http.StatusOK, sh)
}

// ---- documents ----

func (s *Server) uploadDocument(w http.ResponseWriter, r *http.Request) {
	shipmentID := r.PathValue("id")
	if _, err := s.store.GetShipment(r.Context(), shipmentID); err != nil {
		notFoundOr500(w, err, "shipment")
		return
	}

	// Cap the whole request before parsing anything.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "upload_invalid", "upload must be multipart and at most 10 MiB", nil)
		return
	}
	defer r.MultipartForm.RemoveAll()

	docType := domain.DocType(r.FormValue("type"))
	if !docType.Valid() {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request",
			domain.ValidationErrors{{Path: "type", Message: "must be commercial_invoice or packing_list"}})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload_invalid", "form field 'file' is required", nil)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload_invalid", "could not read file", nil)
		return
	}
	if len(data) == 0 || len(data) > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "upload_invalid", "file must be 1 byte to 10 MiB", nil)
		return
	}
	contentType := http.DetectContentType(data)
	if !allowedContent[contentType] {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "only PDF, PNG and JPEG are accepted", nil)
		return
	}

	sum := sha256.Sum256(data)
	doc := domain.Document{
		ID:          newID("doc"),
		ShipmentID:  shipmentID,
		Type:        docType,
		Filename:    filepath.Base(header.Filename), // display only
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		Status:      domain.StatusUploaded,
		UploadedAt:  s.now(),
	}
	doc.StorageKey = doc.ID // storage key is ours, not the user's filename

	if err := s.blobs.Put(r.Context(), doc.StorageKey, bytes.NewReader(data)); err != nil {
		slog.Error("store blob", "request_id", requestIDFrom(r.Context()), "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not store file", nil)
		return
	}
	if err := s.store.CreateDocument(r.Context(), doc); err != nil {
		notFoundOr500(w, err, "document")
		return
	}
	s.audit(r, shipmentID, "document.uploaded", "document", doc.ID,
		map[string]string{"type": string(doc.Type), "sha256": doc.SHA256})
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	shipmentID := r.PathValue("id")
	if _, err := s.store.GetShipment(r.Context(), shipmentID); err != nil {
		notFoundOr500(w, err, "shipment")
		return
	}
	docs, err := s.store.ListDocuments(r.Context(), shipmentID)
	if err != nil {
		notFoundOr500(w, err, "documents")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// ---- extractions ----

// submitExtraction accepts an extraction for a document and validates it
// against the schema for that document's type. Today the JSON is written by
// hand; on day 3 the same endpoint's logic is called with Claude's output.
func (s *Server) submitExtraction(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.GetDocument(r.Context(), r.PathValue("id"))
	if err != nil {
		notFoundOr500(w, err, "document")
		return
	}
	var in domain.ExtractionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Source == "" {
		in.Source = "manual"
	}

	ext := domain.Extraction{ID: newID("ext"), DocumentID: doc.ID, Source: in.Source, CreatedAt: s.now()}
	var vErr error
	switch doc.Type {
	case domain.DocCommercialInvoice:
		var inv domain.InvoiceExtraction
		if err := strictUnmarshal(in.Payload, &inv); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_payload", err.Error(), nil)
			return
		}
		vErr, ext.Invoice = inv.Validate(), &inv
	case domain.DocPackingList:
		var pl domain.PackingListExtraction
		if err := strictUnmarshal(in.Payload, &pl); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_payload", err.Error(), nil)
			return
		}
		vErr, ext.PackingList = pl.Validate(), &pl
	}

	var ve domain.ValidationErrors
	if errors.As(vErr, &ve) {
		s.audit(r, doc.ShipmentID, "extraction.rejected", "document", doc.ID,
			map[string]string{"source": in.Source, "errors": strconv.Itoa(len(ve))})
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "extraction does not match schema", ve)
		return
	}

	if err := s.store.AddExtraction(r.Context(), ext); err != nil {
		notFoundOr500(w, err, "extraction")
		return
	}
	if err := s.store.UpdateDocumentStatus(r.Context(), doc.ID, domain.StatusExtracted); err != nil {
		notFoundOr500(w, err, "document")
		return
	}
	s.audit(r, doc.ShipmentID, "extraction.accepted", "extraction", ext.ID,
		map[string]string{"document_id": doc.ID, "source": in.Source})
	writeJSON(w, http.StatusCreated, ext)
}

func (s *Server) listExtractions(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.GetDocument(r.Context(), r.PathValue("id"))
	if err != nil {
		notFoundOr500(w, err, "document")
		return
	}
	exts, err := s.store.ListExtractions(r.Context(), doc.ID)
	if err != nil {
		notFoundOr500(w, err, "extractions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"extractions": exts})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	shipmentID := r.PathValue("id")
	if _, err := s.store.GetShipment(r.Context(), shipmentID); err != nil {
		notFoundOr500(w, err, "shipment")
		return
	}
	events, err := s.store.ListAudit(r.Context(), shipmentID)
	if err != nil {
		notFoundOr500(w, err, "audit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func strictUnmarshal(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("payload is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
