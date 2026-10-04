package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zeussilver/tradedoc/internal/blob"
	"github.com/zeussilver/tradedoc/internal/domain"
)

const (
	maxUploadBytes = 10 << 20 // 10 MiB, the most we accept for any file
	maxImageBytes  = 5 << 20  // 5 MiB for PNG/JPEG
	maxFilenameLen = 200      // characters; the name is only shown in the UI
	// The browser uses an upload URL straight away, so it only needs to live
	// a few minutes; a leaked URL is useless soon after.
	uploadURLTTL = 5 * time.Minute
)

// Allowed upload types, in the order the UI lists them. The type is always
// decided by sniffing the file's first bytes, never by trusting the client's
// Content-Type header or the file extension.
var allowedTypes = []string{"application/pdf", "image/png", "image/jpeg"}

// maxBytesFor gives the size cap for a content type. Images get a lower cap
// than PDFs: the Claude API limits the size of each image it accepts, and a
// clear scan of one page is far smaller anyway.
func maxBytesFor(contentType string) int64 {
	if contentType == "application/pdf" {
		return maxUploadBytes
	}
	return maxImageBytes
}

// cleanFilename keeps only the last part of a path. Files are stored under
// our own random key, so a name like "../../etc/passwd" can't reach anything,
// but there is no reason to keep or show a path.
func cleanFilename(errs *domain.ValidationErrors, raw string) string {
	name := filepath.Base(strings.TrimSpace(raw))
	if name == "." || name == string(filepath.Separator) {
		name = ""
	}
	checkText(errs, "filename", name, 1, maxFilenameLen, false)
	return name
}

// ---- direct upload: presign, browser PUT, complete ----

type startUploadReq struct {
	Type        domain.DocType `json:"type"`
	Filename    string         `json:"filename"`
	ContentType string         `json:"content_type"`
	SizeBytes   int64          `json:"size_bytes"`
}

type startUploadResp struct {
	Document domain.Document      `json:"document"`
	Upload   blob.PresignedUpload `json:"upload"`
}

// startUpload checks what the client says it will upload and returns a
// short-lived URL to PUT it to. The claims are checked again against the
// real bytes in completeUpload; this first check just fails fast.
func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	var req startUploadReq
	if !decodeJSON(w, r, &req) {
		return
	}

	var errs domain.ValidationErrors
	if !req.Type.Valid() {
		errs = append(errs, domain.FieldError{Path: "type", Message: "must be commercial_invoice or packing_list"})
	}
	name := cleanFilename(&errs, req.Filename)
	if !slices.Contains(allowedTypes, req.ContentType) {
		errs = append(errs, domain.FieldError{Path: "content_type", Message: "must be application/pdf, image/png or image/jpeg"})
	} else if capBytes := maxBytesFor(req.ContentType); req.SizeBytes < 1 || req.SizeBytes > capBytes {
		errs = append(errs, domain.FieldError{Path: "size_bytes",
			Message: fmt.Sprintf("must be 1 to %d bytes for %s", capBytes, req.ContentType)})
	}
	if len(errs) > 0 {
		validationFailed(w, errs)
		return
	}

	doc := domain.Document{
		ID:          newID("doc"),
		ShipmentID:  shipmentID,
		Type:        req.Type,
		Filename:    name, // display only
		ContentType: req.ContentType,
		SizeBytes:   req.SizeBytes,
		Status:      domain.StatusPendingUpload,
		UploadedAt:  s.now(), // when the upload started; not changed later, so ordering stays stable
	}
	doc.StorageKey = doc.ID // storage key is ours, not the user's filename

	// Sign the URL before saving the document: signing has no side effects,
	// so if it fails nothing is left half-created.
	up, err := s.blobs.PresignPut(r.Context(), doc.StorageKey, doc.ContentType, doc.SizeBytes, uploadURLTTL)
	if err != nil {
		slog.Error("presign upload", "request_id", requestIDFrom(r.Context()), "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not prepare upload", nil)
		return
	}
	if err := s.store.CreateDocument(r.Context(), doc); err != nil {
		storeError(w, r, err, "document")
		return
	}
	s.audit(r, shipmentID, "document.upload_started", "document", doc.ID, map[string]string{
		"type": string(doc.Type), "content_type": doc.ContentType, "size_bytes": strconv.FormatInt(doc.SizeBytes, 10),
	})
	writeJSON(w, http.StatusCreated, startUploadResp{Document: doc, Upload: up})
}

// completeUpload is called after the browser's PUT. The browser could have
// sent anything to the URL, so we check the stored bytes ourselves: the size
// must match what was declared (and signed), and the sniffed type must match
// the declared type. Anything else is deleted.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.GetDocument(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, r, err, "document")
		return
	}
	if doc.Status != domain.StatusPendingUpload {
		writeError(w, http.StatusConflict, "conflict", "document is not waiting for an upload", nil)
		return
	}

	size, err := s.blobs.Size(r.Context(), doc.StorageKey)
	switch {
	case errors.Is(err, blob.ErrNotFound):
		s.rejectUpload(w, r, doc, "file_missing", "no file was uploaded")
		return
	case err != nil:
		s.blobError(w, r, err)
		return
	case size != doc.SizeBytes:
		s.rejectUpload(w, r, doc, "size_mismatch", "the uploaded file is not the declared size")
		return
	}

	// Get refuses to read more than the declared size, so a file swapped for
	// a bigger one in the meantime can't make us load it into memory.
	data, err := s.blobs.Get(r.Context(), doc.StorageKey, doc.SizeBytes)
	switch {
	case errors.Is(err, blob.ErrNotFound):
		s.rejectUpload(w, r, doc, "file_missing", "no file was uploaded")
		return
	case errors.Is(err, blob.ErrTooLarge) || (err == nil && int64(len(data)) != doc.SizeBytes):
		s.rejectUpload(w, r, doc, "size_mismatch", "the uploaded file is not the declared size")
		return
	case err != nil:
		s.blobError(w, r, err)
		return
	}
	if http.DetectContentType(data) != doc.ContentType {
		s.rejectUpload(w, r, doc, "type_mismatch", "the file's contents are not "+doc.ContentType)
		return
	}

	sum := sha256.Sum256(data)
	doc.SHA256 = hex.EncodeToString(sum[:])
	doc.Status = domain.StatusUploaded
	if err := s.store.UpdateDocument(r.Context(), doc); err != nil {
		storeError(w, r, err, "document")
		return
	}
	s.audit(r, doc.ShipmentID, "document.uploaded", "document", doc.ID,
		map[string]string{"type": string(doc.Type), "sha256": doc.SHA256})
	writeJSON(w, http.StatusOK, doc)
}

// rejectUpload deletes a file that failed a check, so nothing unchecked stays
// in storage, marks the document rejected and records why (a short reason
// code, never the file's contents).
func (s *Server) rejectUpload(w http.ResponseWriter, r *http.Request, doc domain.Document, reason, msg string) {
	if err := s.blobs.Delete(r.Context(), doc.StorageKey); err != nil && !errors.Is(err, blob.ErrNotFound) {
		slog.Error("delete rejected upload", "request_id", requestIDFrom(r.Context()), "document_id", doc.ID, "err", err)
	}
	doc.Status = domain.StatusRejected
	if err := s.store.UpdateDocument(r.Context(), doc); err != nil {
		storeError(w, r, err, "document")
		return
	}
	s.audit(r, doc.ShipmentID, "document.rejected", "document", doc.ID, map[string]string{"reason": reason})
	writeError(w, http.StatusUnprocessableEntity, "upload_invalid", msg, map[string]string{"reason": reason})
}

func (s *Server) blobError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("blob storage", "request_id", requestIDFrom(r.Context()), "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "file storage error", nil)
}

var errFileChanged = errors.New("stored file does not match the checked upload")

// readChecked loads a document's file and makes sure it is still the file
// that passed completeUpload. An upload URL stays valid for a few minutes, so
// whoever holds it could overwrite the file after it was checked; comparing
// the SHA-256 catches that before the file reaches the model.
func (s *Server) readChecked(ctx context.Context, doc domain.Document) ([]byte, error) {
	data, err := s.blobs.Get(ctx, doc.StorageKey, doc.SizeBytes)
	if errors.Is(err, blob.ErrTooLarge) {
		return nil, errFileChanged
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != doc.SHA256 {
		return nil, errFileChanged
	}
	return data, nil
}

// ---- multipart upload (file passes through this server) ----

// uploadDocument is only registered in local development (see Handler).
func (s *Server) uploadDocument(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
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
		validationFailed(w, domain.ValidationErrors{{Path: "type", Message: "must be commercial_invoice or packing_list"}})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload_invalid", "form field 'file' is required", nil)
		return
	}
	defer file.Close()

	var errs domain.ValidationErrors
	name := cleanFilename(&errs, header.Filename)
	if len(errs) > 0 {
		validationFailed(w, errs)
		return
	}

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
	if !slices.Contains(allowedTypes, contentType) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "only PDF, PNG and JPEG are accepted", nil)
		return
	}
	if int64(len(data)) > maxBytesFor(contentType) {
		writeError(w, http.StatusRequestEntityTooLarge, "upload_invalid", "images must be at most 5 MiB", nil)
		return
	}

	sum := sha256.Sum256(data)
	doc := domain.Document{
		ID:          newID("doc"),
		ShipmentID:  shipmentID,
		Type:        docType,
		Filename:    name, // display only
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		Status:      domain.StatusUploaded,
		UploadedAt:  s.now(),
	}
	doc.StorageKey = doc.ID // storage key is ours, not the user's filename

	if err := s.blobs.Put(r.Context(), doc.StorageKey, bytes.NewReader(data)); err != nil {
		s.blobError(w, r, err)
		return
	}
	if err := s.store.CreateDocument(r.Context(), doc); err != nil {
		storeError(w, r, err, "document")
		return
	}
	s.audit(r, shipmentID, "document.uploaded", "document", doc.ID,
		map[string]string{"type": string(doc.Type), "sha256": doc.SHA256})
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	docs, err := s.store.ListDocuments(r.Context(), shipmentID)
	if err != nil {
		storeError(w, r, err, "documents")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": orEmpty(docs)})
}
