package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// readBoth uploads an invoice and a packing list and types in a reading of
// each, so the shipment is ready for checks.
func readBoth(t *testing.T, e *testEnv, shipID string) (inv, pl domain.Document, invExt, plExt domain.Extraction) {
	t.Helper()
	inv = e.upload(shipID, domain.DocCommercialInvoice, fakePDF)
	invExt = e.submitManual(inv.ID, invoiceFixture())
	pl = e.upload(shipID, domain.DocPackingList, fakePDF)
	plExt = e.submitManual(pl.ID, packingFixture())
	return inv, pl, invExt, plExt
}

func runChecks(t *testing.T, e *testEnv, shipID string) domain.CheckRun {
	t.Helper()
	var run domain.CheckRun
	e.mustDo("POST", "/v1/shipments/"+shipID+"/checks", nil, http.StatusCreated).decode(t, &run)
	return run
}

type errorBody struct {
	Error struct {
		Code    string
		Details []domain.FieldError
	}
}

// Checks and candidate codes both work from the newest document of each
// type. If that document hasn't been read, they refuse (409 not_ready)
// rather than quietly using an older document the user has replaced.
func TestNotReadyUntilNewestDocumentIsRead(t *testing.T) {
	tests := []struct {
		name string
		// newest adds the newest invoice on top of an older one that has
		// been read. nil: no invoice at all.
		newest  func(e *testEnv, shipID string)
		wantMsg string
	}{
		{"no invoice uploaded", nil, "no invoice has been uploaded yet"},
		{"newest upload not completed", func(e *testEnv, shipID string) {
			e.startUpload(shipID, domain.DocCommercialInvoice, "application/pdf", int64(len(fakePDF)), fakePDF)
		}, "the newest invoice upload has not been completed"},
		{"newest upload rejected", func(e *testEnv, shipID string) {
			doc := e.startUpload(shipID, domain.DocCommercialInvoice, "application/pdf", 999, fakePDF) // wrong size
			e.mustDo("POST", "/v1/documents/"+doc.ID+"/complete", nil, http.StatusUnprocessableEntity)
		}, "the newest invoice upload was rejected"},
		{"newest invoice not read", func(e *testEnv, shipID string) {
			e.upload(shipID, domain.DocCommercialInvoice, fakePNG)
		}, "the newest invoice has not been read yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, withModel(newFakeModel()))
			shipID := e.createShipment("JOB-030")
			pl := e.upload(shipID, domain.DocPackingList, fakePDF)
			e.submitManual(pl.ID, packingFixture())
			if tt.newest != nil {
				older := e.upload(shipID, domain.DocCommercialInvoice, fakePDF)
				e.submitManual(older.ID, invoiceFixture())
				tt.newest(e, shipID)
			}

			for _, path := range []string{"/v1/shipments/" + shipID + "/checks", "/v1/shipments/" + shipID + "/suggestions"} {
				var body errorBody
				e.mustDo("POST", path, nil, http.StatusConflict).decode(t, &body)
				if body.Error.Code != "not_ready" || len(body.Error.Details) != 1 ||
					body.Error.Details[0].Path != "commercial_invoice" ||
					!strings.HasPrefix(body.Error.Details[0].Message, tt.wantMsg) {
					t.Fatalf("%s: got %+v", path, body.Error)
				}
			}
		})
	}
}

func TestChecksNotReadyListsEveryMissingDocument(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-031")
	var body errorBody
	e.mustDo("POST", "/v1/shipments/"+shipID+"/checks", nil, http.StatusConflict).decode(t, &body)
	if len(body.Error.Details) != 2 || body.Error.Details[0].Path != "commercial_invoice" || body.Error.Details[1].Path != "packing_list" {
		t.Fatalf("details = %+v", body.Error.Details)
	}
}

// Checks use the newest reading of the newest document of each type.
func TestChecksUseCurrentReadings(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-032")
	_, _, inv1Ext, plExt := readBoth(t, e, shipID)
	if run := runChecks(t, e, shipID); run.InvoiceExtractionID != inv1Ext.ID || run.PackingListExtractionID != plExt.ID {
		t.Fatalf("run = %+v, want invoice %s", run, inv1Ext.ID)
	}

	// A newer invoice wins once it is read; a second reading of it wins over
	// the first.
	inv2 := e.upload(shipID, domain.DocCommercialInvoice, fakePNG)
	e.submitManual(inv2.ID, invoiceFixture())
	inv2Ext2 := e.submitManual(inv2.ID, invoiceFixture())
	if run := runChecks(t, e, shipID); run.InvoiceExtractionID != inv2Ext2.ID {
		t.Fatalf("run = %+v, want invoice %s", run, inv2Ext2.ID)
	}

	var list struct {
		CheckRuns []domain.CheckRun `json:"check_runs"`
	}
	e.mustDo("GET", "/v1/shipments/"+shipID+"/checks", nil, http.StatusOK).decode(t, &list)
	if len(list.CheckRuns) != 2 || list.CheckRuns[0].InvoiceExtractionID != inv1Ext.ID {
		t.Fatalf("check runs (oldest first) = %+v", list.CheckRuns)
	}
}

// seedSuggestion puts a pending candidate code straight into the store, so
// review rules can be tested without a model.
func seedSuggestion(t *testing.T, e *testEnv, shipID, extractionID string, lineNo, rank int, hsCode string) domain.ClassificationSuggestion {
	t.Helper()
	sg := domain.ClassificationSuggestion{
		ID: newID("sug"), ShipmentID: shipID, DocumentID: "doc_x", ExtractionID: extractionID,
		LineNo: lineNo, Description: "Stoneware coffee mug, 350 ml", Rank: rank, HSCode: hsCode,
		Rationale: "test rationale", Confidence: domain.ConfidenceMedium, MissingInfo: []string{},
		Source: "model:fake", Status: domain.SuggestionPending, CreatedAt: time.Now().UTC(),
	}
	if err := e.store.AddSuggestions(t.Context(), []domain.ClassificationSuggestion{sg}); err != nil {
		t.Fatal(err)
	}
	return sg
}

func review(t *testing.T, e *testEnv, sugID, decision string, want int) response {
	t.Helper()
	return e.mustDo("POST", "/v1/suggestions/"+sugID+"/review",
		map[string]string{"decision": decision, "reviewer": "Alex Broker"}, want)
}

func TestReviewSuggestion(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-033")
	sg := seedSuggestion(t, e, shipID, "ext_x", 2, 1, "691200")
	path := "/v1/suggestions/" + sg.ID + "/review"

	invalid := []struct {
		name string
		body map[string]string
	}{
		{"bad decision", map[string]string{"decision": "maybe", "reviewer": "Alex"}},
		{"no reviewer", map[string]string{"decision": "accepted", "reviewer": "   "}},
		{"reviewer too long", map[string]string{"decision": "accepted", "reviewer": strings.Repeat("a", 65)}},
		{"reviewer with newline", map[string]string{"decision": "accepted", "reviewer": "Alex\nadmin"}},
		{"note too long", map[string]string{"decision": "accepted", "reviewer": "Alex", "note": strings.Repeat("n", 501)}},
		{"escape code in note", map[string]string{"decision": "accepted", "reviewer": "Alex", "note": "ok\x1b[2J"}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if r := e.mustDo("POST", path, tt.body, http.StatusUnprocessableEntity); r.errCode(t) != "validation_failed" {
				t.Fatalf("got %s", r.raw)
			}
		})
	}

	ok := map[string]string{"decision": "accepted", "reviewer": "Alex Broker", "note": "matches heading 6912.\nSee supplier spec sheet."}
	e.mustDo("POST", path, ok, http.StatusOK)
	// Decisions are final.
	if r := e.mustDo("POST", path, map[string]string{"decision": "rejected", "reviewer": "Sam"}, http.StatusConflict); r.errCode(t) != "conflict" {
		t.Fatalf("got %s", r.raw)
	}
	e.mustDo("POST", "/v1/suggestions/sug_missing/review", ok, http.StatusNotFound)

	events := e.audit(shipID)
	last := events[len(events)-1]
	if last.Action != "suggestion.accepted" || last.Actor != "Alex Broker" ||
		last.Detail["line_no"] != "2" || last.Detail["rank"] != "1" || last.Detail["hs_code"] != "691200" || len(last.Detail) != 3 {
		t.Fatalf("audit = %+v", last)
	}
}

// A line has exactly one classification: once one candidate for a line is
// accepted, accepting another for the same line (same reading) is refused.
func TestLineAlreadyAccepted(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-034")
	// Illustrative codes only: two candidates from headings that exclude
	// each other (porcelain vs other ceramic).
	first := seedSuggestion(t, e, shipID, "ext_1", 1, 1, "691110")
	second := seedSuggestion(t, e, shipID, "ext_1", 1, 2, "691200")
	otherLine := seedSuggestion(t, e, shipID, "ext_1", 2, 1, "691200")
	sameLineOtherReading := seedSuggestion(t, e, shipID, "ext_2", 1, 1, "691200")

	review(t, e, first.ID, "accepted", http.StatusOK)
	if r := review(t, e, second.ID, "accepted", http.StatusConflict); r.errCode(t) != "line_already_accepted" {
		t.Fatalf("got %s", r.raw)
	}
	if got, _ := e.store.GetSuggestion(t.Context(), second.ID); got.Status != domain.SuggestionPending {
		t.Fatalf("refused candidate status = %s, want pending", got.Status)
	}
	// Rejecting it is still allowed, and other lines and readings are separate.
	review(t, e, second.ID, "rejected", http.StatusOK)
	review(t, e, otherLine.ID, "accepted", http.StatusOK)
	review(t, e, sameLineOtherReading.ID, "accepted", http.StatusOK)

	want := "shipment.created,suggestion.accepted,suggestion.rejected,suggestion.accepted,suggestion.accepted"
	if got := actions(e.audit(shipID)); got != want {
		t.Fatalf("audit = %s, want %s", got, want)
	}
}

func TestSignOffRules(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-035")
	path := "/v1/shipments/" + shipID + "/signoffs"
	signOff := func(decision, runID, note string, want int) response {
		t.Helper()
		return e.mustDo("POST", path, map[string]string{
			"decision": decision, "reviewer": "Alex Broker", "check_run_id": runID, "note": note,
		}, want)
	}

	// No checks yet.
	if r := signOff("approved", "chk_none", "", http.StatusConflict); r.errCode(t) != "not_ready" {
		t.Fatalf("got %s", r.raw)
	}

	// Two runs over the current readings; the newer one has an error
	// finding. Runs are added straight to the store so this doesn't depend
	// on what the checks package finds.
	_, _, invExt, plExt := readBoth(t, e, shipID)
	now := time.Now().UTC()
	older := domain.CheckRun{ID: newID("chk"), ShipmentID: shipID, InvoiceExtractionID: invExt.ID,
		PackingListExtractionID: plExt.ID, Findings: []domain.Finding{}, CreatedAt: now}
	latest := domain.CheckRun{ID: newID("chk"), ShipmentID: shipID, InvoiceExtractionID: invExt.ID,
		PackingListExtractionID: plExt.ID, CreatedAt: now.Add(time.Second), Findings: []domain.Finding{
			{Code: "quantity_mismatch", Severity: domain.SeverityError, Message: "test", LineNo: 2, Refs: []domain.Ref{}},
			{Code: "description_differs", Severity: domain.SeverityInfo, Message: "test", LineNo: 2, Refs: []domain.Ref{}},
		}}
	for _, run := range []domain.CheckRun{older, latest} {
		if err := e.store.AddCheckRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}

	// Signing off against an out-of-date run is refused.
	var stale struct {
		Error struct {
			Code    string
			Details map[string]string
		}
	}
	signOff("approved", older.ID, "ok", http.StatusConflict).decode(t, &stale)
	if stale.Error.Code != "stale_check_run" || stale.Error.Details["reason"] != "newer_run" ||
		stale.Error.Details["latest_check_run_id"] != latest.ID {
		t.Fatalf("got %+v", stale.Error)
	}

	// Approval waits until every candidate code has been reviewed.
	sg := seedSuggestion(t, e, shipID, invExt.ID, 2, 1, "691200")
	if r := signOff("approved", latest.ID, "ok", http.StatusConflict); r.errCode(t) != "suggestions_pending" {
		t.Fatalf("got %s", r.raw)
	}
	review(t, e, sg.ID, "rejected", http.StatusOK)

	// Approving with an open error needs a written reason.
	if r := signOff("approved", latest.ID, "  ", http.StatusUnprocessableEntity); r.errCode(t) != "validation_failed" {
		t.Fatalf("got %s", r.raw)
	}
	// Sending it back needs no reason.
	var back domain.SignOff
	signOff("changes_requested", latest.ID, "", http.StatusCreated).decode(t, &back)
	if back.OpenErrors != 1 {
		t.Fatalf("open errors = %d, want 1", back.OpenErrors)
	}
	var so domain.SignOff
	signOff("approved", latest.ID, "supplier confirmed 780 pcs shipped; invoice to be amended", http.StatusCreated).decode(t, &so)
	if so.Decision != domain.SignOffApproved || so.OpenErrors != 1 || so.Reviewer != "Alex Broker" {
		t.Fatalf("sign-off = %+v", so)
	}

	events := e.audit(shipID)
	last := events[len(events)-1]
	if last.Action != "shipment.signed_off" || last.Actor != "Alex Broker" || last.Detail["decision"] != "approved" ||
		last.Detail["check_run_id"] != latest.ID || last.Detail["open_errors"] != "1" || len(last.Detail) != 3 {
		t.Fatalf("audit = %+v", last)
	}
}

// The newest check run is stale too once the readings it compared are no
// longer the current ones, even if nobody has run the checks again.
func TestSignOffRefusesRunOverOldReadings(t *testing.T) {
	tests := []struct {
		name   string
		change func(e *testEnv, shipID string, inv, pl domain.Document)
		// rerun is what running the checks again then answers: 201 once the
		// new reading exists, 409 not_ready while the newest file is unread.
		rerun int
	}{
		{"invoice read again", func(e *testEnv, _ string, inv, _ domain.Document) {
			e.submitManual(inv.ID, invoiceFixture())
		}, http.StatusCreated},
		{"packing list read again", func(e *testEnv, _ string, _, pl domain.Document) {
			e.submitManual(pl.ID, packingFixture())
		}, http.StatusCreated},
		{"new invoice uploaded, not read", func(e *testEnv, shipID string, _, _ domain.Document) {
			e.upload(shipID, domain.DocCommercialInvoice, fakePNG)
		}, http.StatusConflict},
		{"new packing list upload rejected", func(e *testEnv, shipID string, _, _ domain.Document) {
			doc := e.startUpload(shipID, domain.DocPackingList, "image/png", int64(len(fakePDF)), fakePDF) // a PDF declared as PNG
			e.mustDo("POST", "/v1/documents/"+doc.ID+"/complete", nil, http.StatusUnprocessableEntity)
		}, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			shipID := e.createShipment("JOB-036")
			inv, pl, _, _ := readBoth(t, e, shipID)
			run := runChecks(t, e, shipID)
			tt.change(e, shipID, inv, pl)

			// Refused for either decision: the run no longer describes the
			// documents on screen.
			for _, decision := range []string{"approved", "changes_requested"} {
				var body struct {
					Error struct {
						Code    string
						Details map[string]string
					}
				}
				e.mustDo("POST", "/v1/shipments/"+shipID+"/signoffs", map[string]string{
					"decision": decision, "reviewer": "Alex Broker", "check_run_id": run.ID, "note": "checked",
				}, http.StatusConflict).decode(t, &body)
				if body.Error.Code != "stale_check_run" || body.Error.Details["reason"] != "readings_changed" ||
					body.Error.Details["latest_check_run_id"] != run.ID {
					t.Fatalf("%s: got %+v", decision, body.Error)
				}
			}
			e.mustDo("POST", "/v1/shipments/"+shipID+"/checks", nil, tt.rerun)
		})
	}
}

// Approval only counts candidate codes made from the invoice reading the
// run checked.
func TestSignOffSuggestionsForRunReading(t *testing.T) {
	e := newEnv(t)
	shipID := e.createShipment("JOB-037")
	inv, _, oldExt, _ := readBoth(t, e, shipID)

	// Codes were reviewed on the first reading (one left pending)...
	reviewed := seedSuggestion(t, e, shipID, oldExt.ID, 1, 1, "691110")
	review(t, e, reviewed.ID, "accepted", http.StatusOK)
	seedSuggestion(t, e, shipID, oldExt.ID, 2, 1, "691200")
	// ...then the invoice was corrected and checked again.
	newExt := e.submitManual(inv.ID, invoiceFixture())
	run := runChecks(t, e, shipID)

	approve := func(want int) response {
		t.Helper()
		return e.mustDo("POST", "/v1/shipments/"+shipID+"/signoffs", map[string]string{
			"decision": "approved", "reviewer": "Alex Broker", "check_run_id": run.ID, "note": "checked",
		}, want)
	}
	if r := approve(http.StatusConflict); r.errCode(t) != "suggestions_stale" {
		t.Fatalf("got %s", r.raw)
	}

	// Codes for the current reading: only its pending one counts, not the
	// one left pending on the old reading.
	current := seedSuggestion(t, e, shipID, newExt.ID, 1, 1, "691110")
	var body struct {
		Error struct {
			Code    string
			Details map[string]int
		}
	}
	approve(http.StatusConflict).decode(t, &body)
	if body.Error.Code != "suggestions_pending" || body.Error.Details["pending"] != 1 {
		t.Fatalf("got %+v", body.Error)
	}
	review(t, e, current.ID, "accepted", http.StatusOK)
	approve(http.StatusCreated)
}
