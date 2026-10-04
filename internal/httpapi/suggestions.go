package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/domain"
	"github.com/zeussilver/tradedoc/internal/store"
)

const (
	maxReviewerLen = 64
	maxNoteLen     = 500
)

func valueOf(f domain.Field[string]) string {
	if f.Value == nil {
		return ""
	}
	return *f.Value
}

// createSuggestions asks the model for candidate HS codes for each line of
// the current invoice reading. These are candidate codes for a licensed
// customs broker to review: every one starts "pending" and only a person
// accepts or rejects it.
func (s *Server) createSuggestions(w http.ResponseWriter, r *http.Request) {
	if s.classifier == nil {
		modelNotConfigured(w)
		return
	}
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	inv, notReady, err := s.currentReading(r.Context(), shipmentID, domain.DocCommercialInvoice)
	if err != nil {
		storeError(w, r, err, "extraction")
		return
	}
	if notReady != "" {
		writeError(w, http.StatusConflict, "not_ready", "candidate codes need the newest invoice to be read",
			domain.ValidationErrors{{Path: "commercial_invoice", Message: notReady}})
		return
	}

	// Only what a tariff heading turns on leaves our system: the description
	// and the unit. Prices, quantities, parties and the country of origin are
	// not sent; origin affects the duty rate and trade preferences, not the
	// HS subheading.
	lines := make([]ai.Line, 0, len(inv.Invoice.LineItems))
	byLine := map[int]domain.InvoiceLine{}
	for _, l := range inv.Invoice.LineItems {
		lines = append(lines, ai.Line{
			LineNo:      l.LineNo,
			Description: valueOf(l.Description),
			Unit:        valueOf(l.Unit),
		})
		byLine[l.LineNo] = l
	}

	if !s.takeModelQuota(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelTimeout)
	defer cancel()
	cands, run, err := s.classifier.SuggestCodes(ctx, lines)
	if err != nil {
		s.modelFailed(w, r, shipmentID, "suggestions.model_failed", "shipment", shipmentID, run, err)
		return
	}

	now := s.now()
	source := "model:" + s.modelLabel(run)
	out := []domain.ClassificationSuggestion{}
	dropped := 0
	for _, c := range cands {
		line, known := byLine[c.LineNo]
		if !known {
			dropped++ // the model answered about a line the invoice doesn't have
			continue
		}
		sug := domain.ClassificationSuggestion{
			ID:           newID("sug"),
			ShipmentID:   shipmentID,
			DocumentID:   inv.DocumentID,
			ExtractionID: inv.ID,
			LineNo:       c.LineNo,
			Description:  valueOf(line.Description), // copied so the reviewer sees which line the codes are for
			Evidence:     line.Description.Evidence,
			Rank:         c.Rank,
			HSCode:       c.HSCode,
			Rationale:    c.Rationale,
			Confidence:   c.Confidence,
			MissingInfo:  orEmpty(c.MissingInfo),
			Source:       source,
			Status:       domain.SuggestionPending,
			CreatedAt:    now,
		}
		if sug.Validate() != nil {
			dropped++
			continue
		}
		out = append(out, sug)
	}
	if dropped > 0 {
		// A count, not the candidates: they quote document text.
		slog.Warn("dropped model candidates", "request_id", requestIDFrom(r.Context()), "dropped", dropped)
	}

	if len(out) > 0 {
		if err := s.store.AddSuggestions(r.Context(), out); err != nil {
			storeError(w, r, err, "suggestions")
			return
		}
	}
	s.audit(r, shipmentID, "suggestions.created", "shipment", shipmentID, map[string]string{
		"count": strconv.Itoa(len(out)), "model": s.modelLabel(run),
	})
	writeJSON(w, http.StatusCreated, map[string]any{"suggestions": out})
}

func (s *Server) listSuggestions(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	sugs, err := s.store.ListSuggestions(r.Context(), shipmentID)
	if err != nil {
		storeError(w, r, err, "suggestions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": orEmpty(sugs)})
}

// ---- review ----

type reviewReq struct {
	Decision domain.SuggestionStatus `json:"decision"`
	Reviewer string                  `json:"reviewer"`
	Note     string                  `json:"note"`
}

// reviewSuggestion records a person's decision on one candidate code. A
// decision is final, so the audit trail can't be rewritten after the fact.
func (s *Server) reviewSuggestion(w http.ResponseWriter, r *http.Request) {
	var req reviewReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Reviewer = strings.TrimSpace(req.Reviewer)
	var errs domain.ValidationErrors
	if req.Decision != domain.SuggestionAccepted && req.Decision != domain.SuggestionRejected {
		errs = append(errs, domain.FieldError{Path: "decision", Message: "must be accepted or rejected"})
	}
	checkText(&errs, "reviewer", req.Reviewer, 1, maxReviewerLen, false)
	checkText(&errs, "note", req.Note, 0, maxNoteLen, true)
	if len(errs) > 0 {
		validationFailed(w, errs)
		return
	}

	// The store only changes a suggestion that is still pending, so two
	// people reviewing at once can't both win. It also refuses to accept a
	// second candidate for a line that already has an accepted one: a line
	// has exactly one classification, and two codes from headings that
	// exclude each other would make the record meaningless. Both checks
	// happen inside the store, in one step, so they can't race.
	sug, err := s.store.ReviewSuggestion(r.Context(), r.PathValue("id"),
		domain.Review{Status: req.Decision, Reviewer: req.Reviewer, Note: req.Note, At: s.now()})
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "suggestion has already been reviewed", nil)
		return
	case errors.Is(err, store.ErrLineAccepted):
		writeError(w, http.StatusConflict, "line_already_accepted",
			"another candidate code for this line is already accepted; a line can have only one", nil)
		return
	case err != nil:
		storeError(w, r, err, "suggestion")
		return
	}
	// Not the note: it is free text and may quote the documents.
	s.auditAs(r, req.Reviewer, sug.ShipmentID, "suggestion."+string(req.Decision), "suggestion", sug.ID, map[string]string{
		"line_no": strconv.Itoa(sug.LineNo), "rank": strconv.Itoa(sug.Rank), "hs_code": sug.HSCode,
	})
	writeJSON(w, http.StatusOK, sug)
}
