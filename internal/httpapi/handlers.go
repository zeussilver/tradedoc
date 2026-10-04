package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zeussilver/tradedoc/internal/checks"
	"github.com/zeussilver/tradedoc/internal/domain"
	"github.com/zeussilver/tradedoc/internal/store"
)

// actor is who the audit log says did something. There is no login yet, so
// it is a fixed label; reviews and sign-offs record the reviewer's name instead.
const actor = "demo-user"

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// configResponse is what the UI reads on load, so its limits and buttons
// match what the server will actually accept.
type configResponse struct {
	ModelEnabled   bool     `json:"model_enabled"`
	Model          string   `json:"model"`
	MaxUploadBytes int64    `json:"max_upload_bytes"`
	MaxImageBytes  int64    `json:"max_image_bytes"`
	AllowedTypes   []string `json:"allowed_types"`
	// Currencies whose minor unit is not 2 decimals (e.g. JPY: 0), from the
	// same table the checks use, so the UI and the checks show amounts alike.
	CurrencyMinorUnits map[string]int `json:"currency_minor_units"`
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, configResponse{
		ModelEnabled:       s.extractor != nil && s.classifier != nil,
		Model:              s.modelName,
		MaxUploadBytes:     maxUploadBytes,
		MaxImageBytes:      maxImageBytes,
		AllowedTypes:       allowedTypes,
		CurrencyMinorUnits: checks.MinorUnitExceptions(),
	})
}

// ---- audit and error helpers ----

func (s *Server) audit(r *http.Request, shipmentID, action, entityType, entityID string, detail map[string]string) {
	s.auditAs(r, actor, shipmentID, action, entityType, entityID, detail)
}

// auditAs records an event for a named person (a reviewer). A failed audit
// write is logged but doesn't fail the request: the action already happened.
func (s *Server) auditAs(r *http.Request, who, shipmentID, action, entityType, entityID string, detail map[string]string) {
	err := s.store.AppendAudit(r.Context(), domain.AuditEvent{
		ID: newID("aud"), At: s.now(), Actor: who, Action: action,
		EntityType: entityType, EntityID: entityID, ShipmentID: shipmentID, Detail: detail,
	})
	if err != nil {
		slog.Error("append audit", "request_id", requestIDFrom(r.Context()), "err", err)
	}
}

// storeError maps store errors to HTTP status codes in one place, so every
// write gets the same answers. Unexpected errors are logged (the client only
// sees "internal error") so a failing database shows up in the logs next to
// the request's ID.
func storeError(w http.ResponseWriter, r *http.Request, err error, what string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", what+" not found", nil)
	case errors.Is(err, store.ErrTooLarge):
		// DynamoDB refuses items over 400 KB. That is a problem with the
		// request's size, not a server fault, so it gets a 413 rather than a 500.
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", what+" is too large to store", nil)
	default:
		slog.Error("store", "request_id", requestIDFrom(r.Context()), "what", what, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}

func validationFailed(w http.ResponseWriter, errs domain.ValidationErrors) {
	writeError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request", errs)
}

// checkText validates a short free-text input such as a reviewer name.
// Length is counted in characters, not bytes, so a name in Chinese gets the
// same allowance as one in English. Control characters (escapes, NUL...) are
// refused; multiline text such as a note may still contain line breaks and
// tabs, but one-line values like a name (which ends up in the audit log) may not.
func checkText(errs *domain.ValidationErrors, path, value string, min, max int, multiline bool) {
	n := utf8.RuneCountInString(value)
	if n < min || n > max {
		*errs = append(*errs, domain.FieldError{Path: path, Message: lengthMessage(min, max)})
		return
	}
	badChar := func(r rune) bool {
		lineBreakOrTab := r == '\n' || r == '\r' || r == '\t'
		return unicode.IsControl(r) && !(multiline && lineBreakOrTab)
	}
	if strings.ContainsFunc(value, badChar) {
		*errs = append(*errs, domain.FieldError{Path: path, Message: "must not contain control characters"})
	}
}

func lengthMessage(min, max int) string {
	if min == 0 {
		return fmt.Sprintf("must be at most %d characters", max)
	}
	return fmt.Sprintf("must be %d-%d characters", min, max)
}

// ---- shipments ----

// maxReferenceLen is in characters, like every other text limit here.
const maxReferenceLen = 64

type createShipmentReq struct {
	Reference string `json:"reference"`
}

func (s *Server) createShipment(w http.ResponseWriter, r *http.Request) {
	var req createShipmentReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Reference = strings.TrimSpace(req.Reference)
	var errs domain.ValidationErrors
	checkText(&errs, "reference", req.Reference, 1, maxReferenceLen, false)
	if len(errs) > 0 {
		validationFailed(w, errs)
		return
	}
	sh := domain.Shipment{ID: newID("shp"), Reference: req.Reference, CreatedAt: s.now()}
	if err := s.store.CreateShipment(r.Context(), sh); err != nil {
		storeError(w, r, err, "shipment")
		return
	}
	s.audit(r, sh.ID, "shipment.created", "shipment", sh.ID, nil)
	writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) getShipment(w http.ResponseWriter, r *http.Request) {
	sh, err := s.store.GetShipment(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, r, err, "shipment")
		return
	}
	writeJSON(w, http.StatusOK, sh)
}

// shipmentExists writes a 404 (or 500) and returns false if the shipment in
// the path doesn't exist, so list routes can't be used to probe other data.
func (s *Server) shipmentExists(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := s.store.GetShipment(r.Context(), id); err != nil {
		storeError(w, r, err, "shipment")
		return "", false
	}
	return id, true
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	events, err := s.store.ListAudit(r.Context(), shipmentID)
	if err != nil {
		storeError(w, r, err, "audit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": orEmpty(events)})
}
