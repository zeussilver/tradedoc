package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/blob"
	"github.com/zeussilver/tradedoc/internal/domain"
)

// readable reports whether a document's file has been stored and checked, so
// it can be read. Pending uploads have no checked file yet; rejected ones
// have been deleted.
func readable(d domain.Document) bool {
	return d.Status == domain.StatusUploaded || d.Status == domain.StatusExtracted
}

func notReadable(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "conflict", "document has no checked file yet (upload pending or rejected)", nil)
}

// saveExtraction stores a valid extraction and marks its document extracted.
func (s *Server) saveExtraction(ctx context.Context, doc domain.Document, ext domain.Extraction) error {
	if err := s.store.AddExtraction(ctx, ext); err != nil {
		return err
	}
	doc.Status = domain.StatusExtracted
	return s.store.UpdateDocument(ctx, doc)
}

// ---- manual extraction ----

// submitExtraction accepts an extraction typed (or corrected) by a person and
// validates it against the schema for that document's type.
func (s *Server) submitExtraction(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.GetDocument(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, r, err, "document")
		return
	}
	if !readable(doc) {
		notReadable(w)
		return
	}
	var in domain.ExtractionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	// "model:..." is reserved for results this server got from the model
	// itself; letting a client claim it would make the audit trail lie about
	// where a reading came from.
	if strings.HasPrefix(in.Source, "model:") || len(in.Source) > 64 {
		validationFailed(w, domain.ValidationErrors{{Path: "source", Message: "must be at most 64 characters and not start with \"model:\""}})
		return
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

	if err := s.saveExtraction(r.Context(), doc, ext); err != nil {
		storeError(w, r, err, "extraction")
		return
	}
	s.audit(r, doc.ShipmentID, "extraction.accepted", "extraction", ext.ID,
		map[string]string{"document_id": doc.ID, "source": in.Source})
	writeJSON(w, http.StatusCreated, ext)
}

func (s *Server) listExtractions(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.GetDocument(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, r, err, "document")
		return
	}
	exts, err := s.store.ListExtractions(r.Context(), doc.ID)
	if err != nil {
		storeError(w, r, err, "extractions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"extractions": orEmpty(exts)})
}

func strictUnmarshal(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("payload is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ---- model extraction ----

// extractWithModel sends a document's file to the model and stores what it
// read. The result is a new extraction next to any earlier ones; a person
// still reviews it, and can correct it through submitExtraction.
func (s *Server) extractWithModel(w http.ResponseWriter, r *http.Request) {
	if s.extractor == nil {
		modelNotConfigured(w)
		return
	}
	doc, err := s.store.GetDocument(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, r, err, "document")
		return
	}
	if !readable(doc) {
		notReadable(w)
		return
	}
	data, err := s.readChecked(r.Context(), doc)
	switch {
	case errors.Is(err, errFileChanged):
		s.rejectUpload(w, r, doc, "file_changed", "the stored file no longer matches the checked upload; upload it again")
		return
	case errors.Is(err, blob.ErrNotFound):
		s.rejectUpload(w, r, doc, "file_missing", "the stored file is missing; upload it again")
		return
	case err != nil:
		s.blobError(w, r, err)
		return
	}

	// Everything cheap has been checked: only now spend a unit of the budget.
	if !s.takeModelQuota(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelTimeout)
	defer cancel()

	// ContentType was confirmed by sniffing the bytes in completeUpload.
	file := ai.File{ContentType: doc.ContentType, Data: data}
	ext := domain.Extraction{ID: newID("ext"), DocumentID: doc.ID}
	var run domain.ModelRun
	var vErr error
	switch doc.Type {
	case domain.DocCommercialInvoice:
		var inv domain.InvoiceExtraction
		inv, run, err = s.extractor.ExtractInvoice(ctx, file)
		ext.Invoice = &inv
		if err == nil {
			vErr = inv.Validate()
		}
	case domain.DocPackingList:
		var pl domain.PackingListExtraction
		pl, run, err = s.extractor.ExtractPackingList(ctx, file)
		ext.PackingList = &pl
		if err == nil {
			vErr = pl.Validate()
		}
	}
	// The extractor promises valid output, but we validate again before
	// storing: the cross-document checks rely on what is stored here.
	var ve domain.ValidationErrors
	if err == nil && errors.As(vErr, &ve) {
		err = &ai.InvalidOutputError{Errs: ve, Run: run}
	}
	if err != nil {
		s.modelFailed(w, r, doc.ShipmentID, "extraction.model_failed", "document", doc.ID, run, err)
		return
	}

	// Timestamped when the reading exists, not when the request began: the
	// call can take minutes, and a reading saved in the meantime (say, a
	// manual correction) was saved first, so it must sort first. The DynamoDB
	// store orders readings by this time, the in-memory one by insertion;
	// this way both agree on which reading is the newest.
	ext.CreatedAt = s.now()
	ext.Source = "model:" + s.modelLabel(run)
	ext.Run = &run
	if err := s.saveExtraction(r.Context(), doc, ext); err != nil {
		storeError(w, r, err, "extraction")
		return
	}
	s.audit(r, doc.ShipmentID, "extraction.model_completed", "extraction", ext.ID, s.runDetail(run))
	writeJSON(w, http.StatusCreated, ext)
}
