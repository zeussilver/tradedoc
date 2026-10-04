package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/zeussilver/tradedoc/internal/domain"
)

type signOffReq struct {
	Decision   domain.SignOffDecision `json:"decision"`
	Reviewer   string                 `json:"reviewer"`
	CheckRunID string                 `json:"check_run_id"`
	Note       string                 `json:"note"`
}

// createSignOff records a person approving a shipment, or sending it back,
// after reading a specific check run. It is the human step the workflow ends
// with, so it refuses a run that is out of date (either decision), and
// refuses to approve while candidate codes for that run's invoice reading
// are unreviewed or were reviewed on an older reading.
func (s *Server) createSignOff(w http.ResponseWriter, r *http.Request) {
	var req signOffReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Reviewer = strings.TrimSpace(req.Reviewer)
	var errs domain.ValidationErrors
	if req.Decision != domain.SignOffApproved && req.Decision != domain.SignOffChangesRequested {
		errs = append(errs, domain.FieldError{Path: "decision", Message: "must be approved or changes_requested"})
	}
	checkText(&errs, "reviewer", req.Reviewer, 1, maxReviewerLen, false)
	checkText(&errs, "note", req.Note, 0, maxNoteLen, true)
	if req.CheckRunID == "" {
		errs = append(errs, domain.FieldError{Path: "check_run_id", Message: "is required"})
	}
	if len(errs) > 0 {
		validationFailed(w, errs)
		return
	}

	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	runs, err := s.store.ListCheckRuns(r.Context(), shipmentID)
	if err != nil {
		storeError(w, r, err, "check runs")
		return
	}
	if len(runs) == 0 {
		writeError(w, http.StatusConflict, "not_ready", "run the checks before signing off", nil)
		return
	}
	// The reviewer must have been looking at the newest run: if the data was
	// re-read and re-checked since, what they saw is out of date.
	latest := runs[len(runs)-1]
	if req.CheckRunID != latest.ID {
		staleRun(w, latest.ID, "newer_run", "a newer check run exists; review it before signing off")
		return
	}
	// The newest run can be out of date too: a document may have been read
	// again, or a new one uploaded, without the checks being run again. The
	// run then compared readings that are no longer the current ones, so its
	// findings may not describe the documents now on screen.
	current, err := s.runIsCurrent(r.Context(), shipmentID, latest)
	if err != nil {
		storeError(w, r, err, "extraction")
		return
	}
	if !current {
		staleRun(w, latest.ID, "readings_changed",
			"the documents were uploaded or read again after this check run; run the checks again")
		return
	}
	openErrors := 0
	for _, f := range latest.Findings {
		if f.Severity == domain.SeverityError {
			openErrors++
		}
	}

	if req.Decision == domain.SignOffApproved {
		sugs, err := s.store.ListSuggestions(r.Context(), shipmentID)
		if err != nil {
			storeError(w, r, err, "suggestions")
			return
		}
		// Only candidate codes for the invoice reading this run checked
		// count. Codes reviewed on an older reading were for lines that may
		// since have been corrected (e.g. "porcelain" re-read as "stoneware"),
		// so they can't stand behind this approval.
		forRun, pending := 0, 0
		for _, sg := range sugs {
			if sg.ExtractionID != latest.InvoiceExtractionID {
				continue
			}
			forRun++
			if sg.Status == domain.SuggestionPending {
				pending++
			}
		}
		if len(sugs) > 0 && forRun == 0 {
			writeError(w, http.StatusConflict, "suggestions_stale",
				"the invoice was read again after its candidate codes were reviewed; suggest codes again before approval", nil)
			return
		}
		if pending > 0 {
			writeError(w, http.StatusConflict, "suggestions_pending",
				"every candidate code must be accepted or rejected before approval",
				map[string]int{"pending": pending})
			return
		}
		// Approving despite errors can be legitimate (e.g. the supplier has
		// confirmed a typo), but the reason must be written down.
		if openErrors > 0 && strings.TrimSpace(req.Note) == "" {
			validationFailed(w, domain.ValidationErrors{{Path: "note",
				Message: "is required to approve while the check run has error findings"}})
			return
		}
	}

	so := domain.SignOff{
		ID:         newID("sgn"),
		ShipmentID: shipmentID,
		CheckRunID: latest.ID,
		Decision:   req.Decision,
		Reviewer:   req.Reviewer,
		Note:       req.Note,
		OpenErrors: openErrors,
		CreatedAt:  s.now(),
	}
	if err := s.store.AddSignOff(r.Context(), so); err != nil {
		storeError(w, r, err, "sign-off")
		return
	}
	s.auditAs(r, req.Reviewer, shipmentID, "shipment.signed_off", "signoff", so.ID, map[string]string{
		"decision": string(so.Decision), "check_run_id": so.CheckRunID, "open_errors": strconv.Itoa(openErrors),
	})
	writeJSON(w, http.StatusCreated, so)
}

// runIsCurrent reports whether a check run compared the current readings:
// the newest reading of the newest invoice and of the newest packing list.
func (s *Server) runIsCurrent(ctx context.Context, shipmentID string, run domain.CheckRun) (bool, error) {
	inv, invNotReady, err := s.currentReading(ctx, shipmentID, domain.DocCommercialInvoice)
	if err != nil {
		return false, err
	}
	pl, plNotReady, err := s.currentReading(ctx, shipmentID, domain.DocPackingList)
	if err != nil {
		return false, err
	}
	return invNotReady == "" && plNotReady == "" &&
		inv.ID == run.InvoiceExtractionID && pl.ID == run.PackingListExtractionID, nil
}

// staleRun refuses a sign-off on a check run that is out of date. reason
// tells the UI which kind: "newer_run" or "readings_changed".
func staleRun(w http.ResponseWriter, latestRunID, reason, msg string) {
	writeError(w, http.StatusConflict, "stale_check_run", msg,
		map[string]string{"latest_check_run_id": latestRunID, "reason": reason})
}

func (s *Server) listSignOffs(w http.ResponseWriter, r *http.Request) {
	shipmentID, ok := s.shipmentExists(w, r)
	if !ok {
		return
	}
	sos, err := s.store.ListSignOffs(r.Context(), shipmentID)
	if err != nil {
		storeError(w, r, err, "sign-offs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signoffs": orEmpty(sos)})
}
