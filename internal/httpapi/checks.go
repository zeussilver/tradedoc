package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/zeussilver/tradedoc/internal/checks"
	"github.com/zeussilver/tradedoc/internal/domain"
)

// currentReading picks what the checks and suggestions work from: the newest
// reading of the newest document of the given type.
//
// If the newest document hasn't been read (its upload is unfinished or was
// rejected, or nobody has read it yet), there is no current reading and
// notReady says why. We never fall back to an older document: the user has
// replaced it, and checking the old file while the new one is on screen
// would report on a document nobody is looking at.
func (s *Server) currentReading(ctx context.Context, shipmentID string, t domain.DocType) (ext domain.Extraction, notReady string, err error) {
	docs, err := s.store.ListDocuments(ctx, shipmentID)
	if err != nil {
		return domain.Extraction{}, "", err
	}
	name := docLabel(t)
	// Store lists are oldest first, so the newest of this type is the last one.
	var newest *domain.Document
	for i := range docs {
		if docs[i].Type == t {
			newest = &docs[i]
		}
	}
	switch {
	case newest == nil:
		return domain.Extraction{}, "no " + name + " has been uploaded yet", nil
	case newest.Status == domain.StatusPendingUpload:
		return domain.Extraction{}, "the newest " + name + " upload has not been completed", nil
	case newest.Status == domain.StatusRejected:
		return domain.Extraction{}, "the newest " + name + " upload was rejected; upload it again", nil
	case newest.Status != domain.StatusExtracted:
		return domain.Extraction{}, "the newest " + name + " has not been read yet", nil
	}

	exts, err := s.store.ListExtractions(ctx, newest.ID)
	if err != nil {
		return domain.Extraction{}, "", err
	}
	for j := len(exts) - 1; j >= 0; j-- {
		// Defensive: only take a reading that matches the document type.
		if (t == domain.DocCommercialInvoice && exts[j].Invoice != nil) ||
			(t == domain.DocPackingList && exts[j].PackingList != nil) {
			return exts[j], "", nil
		}
	}
	return domain.Extraction{}, "the newest " + name + " has not been read yet", nil
}

// docLabel is how messages name a document type.
func docLabel(t domain.DocType) string {
	if t == domain.DocPackingList {
		return "packing list"
	}
	return "invoice"
}

// runChecks compares the current invoice reading with the current packing
// list reading and stores the result as a new, append-only check run.
func (s *Server) runChecks(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	inv, invNotReady, err := s.currentReading(r.Context(), shipmentID, domain.DocCommercialInvoice)
	if err != nil {
		storeError(w, r, err, "extraction")
		return
	}
	pl, plNotReady, err := s.currentReading(r.Context(), shipmentID, domain.DocPackingList)
	if err != nil {
		storeError(w, r, err, "extraction")
		return
	}
	if invNotReady != "" || plNotReady != "" {
		var missing domain.ValidationErrors
		if invNotReady != "" {
			missing = append(missing, domain.FieldError{Path: "commercial_invoice", Message: invNotReady})
		}
		if plNotReady != "" {
			missing = append(missing, domain.FieldError{Path: "packing_list", Message: plNotReady})
		}
		writeError(w, http.StatusConflict, "not_ready", "checks need the newest invoice and the newest packing list to be read", missing)
		return
	}

	run := domain.CheckRun{
		ID:                      newID("chk"),
		ShipmentID:              shipmentID,
		InvoiceExtractionID:     inv.ID,
		PackingListExtractionID: pl.ID,
		Findings:                orEmpty(checks.Compare(inv, pl)),
		CreatedAt:               s.now(),
	}
	if err := s.store.AddCheckRun(r.Context(), run); err != nil {
		storeError(w, r, err, "check run")
		return
	}

	// Counts only: finding messages quote document values.
	counts := map[domain.Severity]int{}
	for _, f := range run.Findings {
		counts[f.Severity]++
	}
	s.audit(r, shipmentID, "checks.completed", "check_run", run.ID, map[string]string{
		"error":   strconv.Itoa(counts[domain.SeverityError]),
		"warning": strconv.Itoa(counts[domain.SeverityWarning]),
		"info":    strconv.Itoa(counts[domain.SeverityInfo]),
	})
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) listCheckRuns(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	runs, err := s.store.ListCheckRuns(r.Context(), shipmentID)
	if err != nil {
		storeError(w, r, err, "check runs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"check_runs": orEmpty(runs)})
}
