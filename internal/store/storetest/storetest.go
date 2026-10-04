// Package storetest is a conformance suite for store.Store. Every Store
// implementation runs the same tests, so the in-memory store used in local
// runs and tests and the DynamoDB store used on AWS are known to behave the
// same way: handlers can't tell which one they were given.
package storetest

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zeussilver/tradedoc/internal/domain"
	"github.com/zeussilver/tradedoc/internal/store"
)

// Run runs every check against stores made by newStore. Each subtest gets a
// fresh, empty store, so tests can't affect each other.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"Shipments", testShipments},
		{"Documents", testDocuments},
		{"UpdateDocument", testUpdateDocument},
		{"Extractions", testExtractions},
		{"CheckRuns", testCheckRuns},
		{"Suggestions", testSuggestions},
		{"ManySuggestions", testManySuggestions},
		{"ReviewSuggestion", testReviewSuggestion},
		{"ConcurrentReview", testConcurrentReview},
		{"LineAccepted", testLineAccepted},
		{"ConcurrentLineAccept", testConcurrentLineAccept},
		{"SignOffs", testSignOffs},
		{"Audit", testAudit},
		{"Quota", testQuota},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, newStore(t)) })
	}
}

var base = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

// at returns the i-th test timestamp. Lists are ordered by time, so records
// get distinct, increasing times. The 250 ms step mixes whole and fractional
// seconds on purpose: a sort key that drops trailing zeros (RFC3339Nano
// writes "09:00:00Z" but "09:00:00.25Z") would sort the later time first
// and fail these tests.
func at(i int) time.Time { return base.Add(time.Duration(i) * 250 * time.Millisecond) }

// ---- shipments and documents ----

func testShipments(t *testing.T, s store.Store) {
	ctx := t.Context()
	want := domain.Shipment{ID: "shp_1", Reference: "JOB-001", CreatedAt: at(0)}
	check(t, s.CreateShipment(ctx, want))

	got, err := s.GetShipment(ctx, "shp_1")
	check(t, err)
	same(t, got, want)

	_, err = s.GetShipment(ctx, "shp_missing")
	wantErr(t, err, store.ErrNotFound)
}

func document(id, shipmentID string, uploaded time.Time) domain.Document {
	return domain.Document{
		ID: id, ShipmentID: shipmentID, Type: domain.DocCommercialInvoice,
		Filename: id + ".pdf", ContentType: "application/pdf", SizeBytes: 1234,
		SHA256:     "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		StorageKey: "uploads/" + id, // tagged json:"-", so a JSON-based store must keep it some other way
		Status:     domain.StatusUploaded, UploadedAt: uploaded,
	}
}

func testDocuments(t *testing.T, s store.Store) {
	ctx := t.Context()
	// Inserted in an order that matches neither upload time nor ID, so the
	// list can only come back right if it is really sorted by upload time.
	d1 := document("doc_1", "shp_1", at(1))
	d3 := document("doc_3", "shp_1", at(0))
	d2 := document("doc_2", "shp_1", at(2))
	other := document("doc_9", "shp_2", at(3))
	for _, d := range []domain.Document{d1, d3, d2, other} {
		check(t, s.CreateDocument(ctx, d))
	}

	got, err := s.ListDocuments(ctx, "shp_1")
	check(t, err)
	same(t, got, []domain.Document{d3, d1, d2})

	doc, err := s.GetDocument(ctx, "doc_2")
	check(t, err)
	same(t, doc, d2)

	_, err = s.GetDocument(ctx, "doc_missing")
	wantErr(t, err, store.ErrNotFound)

	none, err := s.ListDocuments(ctx, "shp_none")
	check(t, err)
	wantEmpty(t, none)
}

func testUpdateDocument(t *testing.T, s store.Store) {
	ctx := t.Context()
	d := document("doc_1", "shp_1", at(0))
	d.Status, d.SizeBytes, d.SHA256 = domain.StatusPendingUpload, 0, ""
	check(t, s.CreateDocument(ctx, d))

	d.Status, d.SizeBytes, d.SHA256 = domain.StatusUploaded, 2048, "abc123"
	check(t, s.UpdateDocument(ctx, d))

	got, err := s.GetDocument(ctx, "doc_1")
	check(t, err)
	same(t, got, d)
	list, err := s.ListDocuments(ctx, "shp_1")
	check(t, err)
	same(t, list, []domain.Document{d}) // replaced, not added a second time

	// Updating a document that was never created must fail, and must not
	// create it as a side effect.
	missing := document("doc_missing", "shp_1", at(1))
	wantErr(t, s.UpdateDocument(ctx, missing), store.ErrNotFound)
	_, err = s.GetDocument(ctx, "doc_missing")
	wantErr(t, err, store.ErrNotFound)
}

// ---- extractions ----

func invoiceExtraction(id, docID string, created time.Time) domain.Extraction {
	return domain.Extraction{
		ID: id, DocumentID: docID, Source: "model:test-model", CreatedAt: created,
		Run: &domain.ModelRun{Model: "test-model", Attempts: 2, InputTokens: 1500, OutputTokens: 400},
		Invoice: &domain.InvoiceExtraction{
			InvoiceNumber: domain.Of("INV-2026-0042", domain.ConfidenceHigh, 1, "Invoice No.: INV-2026-0042"),
			Currency:      domain.Of("USD", domain.ConfidenceHigh, 1, "Currency: USD"),
			Incoterm:      domain.Field[string]{}, // not found: must stay null, not become ""
			LineItems: []domain.InvoiceLine{{
				LineNo:         1,
				Description:    domain.Of("Porcelain dinner plate, 27 cm", domain.ConfidenceHigh, 1, "Porcelain dinner plate, 27 cm"),
				Quantity:       domain.Of("1200", domain.ConfidenceMedium, 1, "1,200 PCS"),
				UnitPrice:      domain.Of("3.50", domain.ConfidenceHigh, 1, "3.50"),
				LineTotalMinor: domain.Of(int64(420000), domain.ConfidenceHigh, 1, "4,200.00"),
			}},
			Charges: []domain.InvoiceCharge{{
				Kind:        domain.ChargeFreight,
				Label:       domain.Of("Ocean freight", domain.ConfidenceHigh, 1, "Ocean freight"),
				AmountMinor: domain.Of(int64(45000), domain.ConfidenceHigh, 1, "450.00"),
			}},
			TotalMinor: domain.Of(int64(730200), domain.ConfidenceLow, 1, "USD 7,302.00"),
		},
	}
}

func packingExtraction(id, docID string, created time.Time) domain.Extraction {
	return domain.Extraction{
		ID: id, DocumentID: docID, Source: "manual", CreatedAt: created,
		PackingList: &domain.PackingListExtraction{
			InvoiceRef:    domain.Of("INV-2026-0042", domain.ConfidenceHigh, 1, "Invoice Ref: INV-2026-0042"),
			TotalPackages: domain.Of(int64(84), domain.ConfidenceHigh, 1, "84 CTNS"),
			LineItems: []domain.PackingLine{
				{LineNo: 2, Quantity: domain.Of("780", domain.ConfidenceHigh, 1, "780 PCS")},
				// The trailing zero is on purpose: a store that turned the
				// quantity into a number would give back "1250.5".
				{LineNo: 3, Quantity: domain.Of("1250.50", domain.ConfidenceHigh, 1, "1,250.50 KG")},
			},
		},
	}
}

func testExtractions(t *testing.T, s store.Store) {
	ctx := t.Context()
	// "doc_1" is a prefix of "doc_10": listing doc_1's extractions must not
	// pick up doc_10's.
	check(t, s.CreateDocument(ctx, document("doc_1", "shp_1", at(0))))
	check(t, s.CreateDocument(ctx, document("doc_10", "shp_1", at(1))))

	// IDs run backwards so ID order and time order disagree.
	first := invoiceExtraction("ext_b", "doc_1", at(2))
	theirs := packingExtraction("ext_c", "doc_10", at(3))
	second := invoiceExtraction("ext_a", "doc_1", at(4))
	for _, e := range []domain.Extraction{first, theirs, second} {
		check(t, s.AddExtraction(ctx, e))
	}

	got, err := s.ListExtractions(ctx, "doc_1")
	check(t, err)
	same(t, got, []domain.Extraction{first, second}) // appended, never overwritten

	got, err = s.ListExtractions(ctx, "doc_10")
	check(t, err)
	same(t, got, []domain.Extraction{theirs})

	none, err := s.ListExtractions(ctx, "doc_none")
	check(t, err)
	wantEmpty(t, none)
}

// ---- check runs ----

func checkRun(id, shipmentID string, created time.Time) domain.CheckRun {
	ev := &domain.Evidence{Page: 1, Quote: "800 PCS"}
	return domain.CheckRun{
		ID: id, ShipmentID: shipmentID, CreatedAt: created,
		InvoiceExtractionID: "ext_1", PackingListExtractionID: "ext_2",
		Findings: []domain.Finding{{
			Code: "quantity_mismatch", Severity: domain.SeverityError, LineNo: 2,
			Message: "Line 2 quantity differs between the invoice and the packing list.",
			Refs: []domain.Ref{
				{DocumentID: "doc_1", Path: "line_items[1].quantity", Value: "800 PCS", Evidence: ev},
				{DocumentID: "doc_2", Path: "line_items[1].quantity", Value: "780 PCS"},
			},
		}},
	}
}

func testCheckRuns(t *testing.T, s store.Store) {
	ctx := t.Context()
	r1 := checkRun("chk_b", "shp_1", at(0))
	other := checkRun("chk_c", "shp_2", at(1))
	r2 := checkRun("chk_a", "shp_1", at(2))
	r2.Findings = []domain.Finding{} // a clean run: no findings
	for _, r := range []domain.CheckRun{r1, other, r2} {
		check(t, s.AddCheckRun(ctx, r))
	}

	got, err := s.ListCheckRuns(ctx, "shp_1")
	check(t, err)
	same(t, got, []domain.CheckRun{r1, r2})

	got, err = s.ListCheckRuns(ctx, "shp_2")
	check(t, err)
	same(t, got, []domain.CheckRun{other})

	none, err := s.ListCheckRuns(ctx, "shp_none")
	check(t, err)
	wantEmpty(t, none)
}

// ---- classification suggestions ----

func suggestion(id, shipmentID string, lineNo, rank int, created time.Time) domain.ClassificationSuggestion {
	return domain.ClassificationSuggestion{
		ID: id, ShipmentID: shipmentID, DocumentID: "doc_1", ExtractionID: "ext_1",
		LineNo: lineNo, Rank: rank, Description: "Porcelain dinner plate, 27 cm",
		Evidence:    &domain.Evidence{Page: 1, Quote: "Porcelain dinner plate, 27 cm"},
		HSCode:      "691110",
		Rationale:   "Heading 6911 covers tableware of porcelain; candidate for a licensed customs broker to review.",
		Confidence:  domain.ConfidenceMedium,
		MissingInfo: []string{"confirm the body is porcelain, not stoneware"},
		Source:      "model:test-model", Status: domain.SuggestionPending, CreatedAt: created,
	}
}

func testSuggestions(t *testing.T, s store.Store) {
	ctx := t.Context()
	s1 := suggestion("sug_b", "shp_1", 1, 1, at(0))
	other := suggestion("sug_c", "shp_2", 1, 1, at(1))
	s2 := suggestion("sug_a", "shp_1", 1, 2, at(2))
	check(t, s.AddSuggestions(ctx, []domain.ClassificationSuggestion{s1, other, s2}))
	check(t, s.AddSuggestions(ctx, nil)) // an empty batch is a no-op, not an error

	got, err := s.ListSuggestions(ctx, "shp_1")
	check(t, err)
	same(t, got, []domain.ClassificationSuggestion{s1, s2})

	one, err := s.GetSuggestion(ctx, "sug_a")
	check(t, err)
	same(t, one, s2)

	_, err = s.GetSuggestion(ctx, "sug_missing")
	wantErr(t, err, store.ErrNotFound)

	none, err := s.ListSuggestions(ctx, "shp_none")
	check(t, err)
	wantEmpty(t, none)
}

// The handler stores all candidates from one model call with the same
// CreatedAt. They must still come back in the order they were added, and a
// batch bigger than one database transaction must be stored completely.
func testManySuggestions(t *testing.T, s store.Store) {
	ctx := t.Context()
	const n = 120
	var batch []domain.ClassificationSuggestion
	for i := range n {
		// i*7 mod 120 visits every number once, so IDs are unique but not
		// in insertion order.
		id := fmt.Sprintf("sug_%03d", (i*7)%n)
		batch = append(batch, suggestion(id, "shp_1", i/3+1, i%3+1, at(0)))
	}
	check(t, s.AddSuggestions(ctx, batch))

	got, err := s.ListSuggestions(ctx, "shp_1")
	check(t, err)
	same(t, got, batch)
}

func testReviewSuggestion(t *testing.T, s store.Store) {
	ctx := t.Context()
	sug := suggestion("sug_1", "shp_1", 1, 1, at(0))
	check(t, s.AddSuggestions(ctx, []domain.ClassificationSuggestion{sug}))

	first := domain.Review{Status: domain.SuggestionAccepted, Reviewer: "Alice", Note: "Matches the product spec sheet.", At: at(5)}
	got, err := s.ReviewSuggestion(ctx, "sug_1", first)
	check(t, err)
	want := sug
	reviewedAt := at(5)
	want.Status, want.ReviewedBy, want.ReviewNote, want.ReviewedAt = domain.SuggestionAccepted, "Alice", first.Note, &reviewedAt
	same(t, got, want)

	stored, err := s.GetSuggestion(ctx, "sug_1")
	check(t, err)
	same(t, stored, want)

	// Decisions are final: a second review is refused and changes nothing.
	second := domain.Review{Status: domain.SuggestionRejected, Reviewer: "Bob", At: at(6)}
	_, err = s.ReviewSuggestion(ctx, "sug_1", second)
	wantErr(t, err, store.ErrConflict)
	stored, err = s.GetSuggestion(ctx, "sug_1")
	check(t, err)
	same(t, stored, want)

	list, err := s.ListSuggestions(ctx, "shp_1")
	check(t, err)
	same(t, list, []domain.ClassificationSuggestion{want})

	_, err = s.ReviewSuggestion(ctx, "sug_missing", first)
	wantErr(t, err, store.ErrNotFound)
}

// Several reviewers deciding the same suggestion at the same moment: exactly
// one must win and the rest must get ErrConflict. This is what stops the
// second decision silently overwriting the first.
func testConcurrentReview(t *testing.T, s store.Store) {
	ctx := t.Context()
	check(t, s.AddSuggestions(ctx, []domain.ClassificationSuggestion{suggestion("sug_1", "shp_1", 1, 1, at(0))}))

	const reviewers = 8
	errs := make([]error, reviewers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range reviewers {
		wg.Go(func() {
			<-start // release everyone together so the calls really overlap
			r := domain.Review{Status: domain.SuggestionAccepted, Reviewer: fmt.Sprintf("reviewer-%d", i), At: at(1 + i)}
			_, errs[i] = s.ReviewSuggestion(ctx, "sug_1", r)
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner == -1:
			winner = i
		case err == nil:
			t.Fatalf("reviewers %d and %d both succeeded", winner, i)
		case !errors.Is(err, store.ErrConflict):
			t.Fatalf("reviewer %d: got %v, want ErrConflict", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no reviewer succeeded")
	}
	stored, err := s.GetSuggestion(ctx, "sug_1")
	check(t, err)
	if want := fmt.Sprintf("reviewer-%d", winner); stored.ReviewedBy != want {
		t.Fatalf("stored reviewer %q, want the winner %q", stored.ReviewedBy, want)
	}
}

// An invoice line gets one classification. Once a candidate for a line is
// accepted, accepting another candidate for the same line (same extraction,
// same line_no) is refused with ErrLineAccepted and changes nothing.
// Rejecting the other candidates is how a reviewer finishes the line, so
// that must keep working, and other lines and other readings of the invoice
// are not affected.
func testLineAccepted(t *testing.T, s store.Store) {
	ctx := t.Context()
	newer := suggestion("sug_new_1_1", "shp_1", 1, 1, at(1))
	newer.ExtractionID = "ext_2" // line 1 again, from a newer reading of the invoice
	check(t, s.AddSuggestions(ctx, []domain.ClassificationSuggestion{
		suggestion("sug_1_1", "shp_1", 1, 1, at(0)), // IDs are sug_<line>_<rank>
		suggestion("sug_1_2", "shp_1", 1, 2, at(0)),
		suggestion("sug_1_3", "shp_1", 1, 3, at(0)),
		suggestion("sug_2_1", "shp_1", 2, 1, at(0)),
		suggestion("sug_2_2", "shp_1", 2, 2, at(0)),
		newer,
	}))

	accept, reject := domain.SuggestionAccepted, domain.SuggestionRejected
	steps := []struct {
		id       string
		decision domain.SuggestionStatus
		want     error
	}{
		{"sug_1_1", accept, nil},
		{"sug_1_2", accept, store.ErrLineAccepted}, // line 1 already has an accepted code
		{"sug_1_3", accept, store.ErrLineAccepted},
		{"sug_1_2", reject, nil}, // rejecting the other candidates is fine
		{"sug_1_3", reject, nil},
		{"sug_1_1", accept, store.ErrConflict}, // already decided: that is reported first
		{"sug_2_1", reject, nil},
		{"sug_2_2", accept, nil},     // a different line; its rejected candidate doesn't count
		{"sug_new_1_1", accept, nil}, // same line number, different extraction
	}
	for i, st := range steps {
		before, err := s.GetSuggestion(ctx, st.id)
		check(t, err)
		_, err = s.ReviewSuggestion(ctx, st.id, domain.Review{Status: st.decision, Reviewer: "Alice", At: at(10 + i)})
		if !errors.Is(err, st.want) {
			t.Fatalf("step %d (%s %s): got error %v, want %v", i+1, st.decision, st.id, err, st.want)
		}
		if st.want != nil {
			after, err := s.GetSuggestion(ctx, st.id)
			check(t, err)
			same(t, after, before) // a refused decision changes nothing
		}
	}

	want := map[string]domain.SuggestionStatus{
		"sug_1_1": accept, "sug_1_2": reject, "sug_1_3": reject,
		"sug_2_1": reject, "sug_2_2": accept,
		"sug_new_1_1": accept,
	}
	list, err := s.ListSuggestions(ctx, "shp_1")
	check(t, err)
	if len(list) != len(want) {
		t.Fatalf("got %d suggestions, want %d", len(list), len(want))
	}
	for _, sug := range list {
		if sug.Status != want[sug.ID] {
			t.Fatalf("%s: stored status %q, want %q", sug.ID, sug.Status, want[sug.ID])
		}
	}
}

// Reviewers accepting different candidates for the same line at the same
// moment: per line exactly one may win, and the others must get
// ErrLineAccepted and stay pending. A store that checks the siblings first
// and writes afterwards, as two separate steps, lets several through here.
func testConcurrentLineAccept(t *testing.T, s store.Store) {
	ctx := t.Context()
	const lines, ranks = 5, 3
	var sugs []domain.ClassificationSuggestion
	for line := 1; line <= lines; line++ {
		for rank := 1; rank <= ranks; rank++ {
			sugs = append(sugs, suggestion(fmt.Sprintf("sug_%d_%d", line, rank), "shp_1", line, rank, at(0)))
		}
	}
	check(t, s.AddSuggestions(ctx, sugs))

	errs := make([]error, len(sugs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, sug := range sugs {
		wg.Go(func() {
			<-start // release everyone together so the calls really overlap
			r := domain.Review{Status: domain.SuggestionAccepted, Reviewer: fmt.Sprintf("reviewer-%d", i), At: at(1)}
			_, errs[i] = s.ReviewSuggestion(ctx, sug.ID, r)
		})
	}
	close(start)
	wg.Wait()

	winners := map[int]string{} // line_no -> the suggestion accepted for it
	for i, sug := range sugs {
		switch {
		case errs[i] == nil && winners[sug.LineNo] != "":
			t.Fatalf("line %d: %s and %s were both accepted", sug.LineNo, winners[sug.LineNo], sug.ID)
		case errs[i] == nil:
			winners[sug.LineNo] = sug.ID
		case !errors.Is(errs[i], store.ErrLineAccepted):
			t.Fatalf("%s: got %v, want ErrLineAccepted", sug.ID, errs[i])
		}
	}
	if len(winners) != lines {
		t.Fatalf("%d of %d lines got an accepted candidate", len(winners), lines)
	}

	// The stored records must agree: the winner accepted, the rest pending.
	list, err := s.ListSuggestions(ctx, "shp_1")
	check(t, err)
	for _, sug := range list {
		want := domain.SuggestionPending
		if winners[sug.LineNo] == sug.ID {
			want = domain.SuggestionAccepted
		}
		if sug.Status != want {
			t.Fatalf("%s: stored status %q, want %q", sug.ID, sug.Status, want)
		}
	}
}

// ---- sign-offs and audit ----

func testSignOffs(t *testing.T, s store.Store) {
	ctx := t.Context()
	back := domain.SignOff{ID: "sgn_b", ShipmentID: "shp_1", CheckRunID: "chk_1", Decision: domain.SignOffChangesRequested,
		Reviewer: "Alice", Note: "Ask the shipper about line 2.", OpenErrors: 2, CreatedAt: at(0)}
	other := domain.SignOff{ID: "sgn_c", ShipmentID: "shp_2", CheckRunID: "chk_9", Decision: domain.SignOffApproved,
		Reviewer: "Carol", CreatedAt: at(1)}
	approved := domain.SignOff{ID: "sgn_a", ShipmentID: "shp_1", CheckRunID: "chk_2", Decision: domain.SignOffApproved,
		Reviewer: "Alice", CreatedAt: at(2)}
	for _, so := range []domain.SignOff{back, other, approved} {
		check(t, s.AddSignOff(ctx, so))
	}

	got, err := s.ListSignOffs(ctx, "shp_1")
	check(t, err)
	same(t, got, []domain.SignOff{back, approved})

	none, err := s.ListSignOffs(ctx, "shp_none")
	check(t, err)
	wantEmpty(t, none)
}

func testAudit(t *testing.T, s store.Store) {
	ctx := t.Context()
	uploaded := domain.AuditEvent{ID: "aud_b", At: at(0), Actor: "anonymous", Action: "document.uploaded",
		EntityType: "document", EntityID: "doc_1", ShipmentID: "shp_1", Detail: map[string]string{"type": "commercial_invoice"}}
	other := domain.AuditEvent{ID: "aud_c", At: at(1), Actor: "anonymous", Action: "shipment.created",
		EntityType: "shipment", EntityID: "shp_2", ShipmentID: "shp_2"}
	checked := domain.AuditEvent{ID: "aud_a", At: at(2), Actor: "anonymous", Action: "checks.completed",
		EntityType: "check_run", EntityID: "chk_1", ShipmentID: "shp_1", Detail: map[string]string{"error": "2", "warning": "1"}}
	for _, e := range []domain.AuditEvent{uploaded, other, checked} {
		check(t, s.AppendAudit(ctx, e))
	}

	got, err := s.ListAudit(ctx, "shp_1")
	check(t, err)
	same(t, got, []domain.AuditEvent{uploaded, checked})

	none, err := s.ListAudit(ctx, "shp_none")
	check(t, err)
	wantEmpty(t, none)
}

// ---- quota ----

func testQuota(t *testing.T, s store.Store) {
	ctx := t.Context()
	take := func(key string, limit int64) bool {
		t.Helper()
		ok, err := s.TakeQuota(ctx, key, limit)
		check(t, err)
		return ok
	}

	for i := range 3 {
		if !take("model_calls:2026-10-04", 3) {
			t.Fatalf("call %d of 3 was refused", i+1)
		}
	}
	if take("model_calls:2026-10-04", 3) {
		t.Fatal("4th call allowed past a limit of 3")
	}
	if !take("model_calls:2026-10-05", 3) {
		t.Fatal("a different key must have its own count")
	}
	if take("model_calls:zero", 0) {
		t.Fatal("a limit of 0 must allow nothing")
	}

	// The quota exists to cap spend across many server instances, so it
	// must hold when calls race: 20 at once against a limit of 5.
	const callers, limit = 20, 5
	allowed := make([]bool, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-start
			allowed[i], errs[i] = s.TakeQuota(ctx, "model_calls:race", limit)
		})
	}
	close(start)
	wg.Wait()
	n := 0
	for i := range callers {
		check(t, errs[i])
		if allowed[i] {
			n++
		}
	}
	if n != limit {
		t.Fatalf("%d concurrent calls were allowed, want exactly %d", n, limit)
	}
}

// ---- helpers ----

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got error %v, want %v", err, want)
	}
}

// same compares whole records, so a store that drops or mangles any field
// (a pointer, a nil-versus-empty value, a timestamp) fails here.
func same[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records differ\n got: %+v\nwant: %+v", got, want)
	}
}

// wantEmpty checks for an empty, non-nil list: a nil slice is encoded as
// JSON null, and the API promises clients an array.
func wantEmpty[T any](t *testing.T, list []T) {
	t.Helper()
	if list == nil || len(list) != 0 {
		t.Fatalf("got %#v, want an empty non-nil list", list)
	}
}
