package checks

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/zeussilver/tradedoc/internal/domain"
)

const (
	invoiceDoc = "doc_invoice"
	packingDoc = "doc_packing"
)

// hi builds a high-confidence value read from page 1.
func hi[T any](v T, quote string) domain.Field[T] {
	return domain.Of(v, domain.ConfidenceHigh, 1, quote)
}

// low builds the same value rated low confidence.
func low[T any](v T, quote string) domain.Field[T] {
	return domain.Of(v, domain.ConfidenceLow, 1, quote)
}

func invLine(no int, desc, qty, price string, totalMinor int64) domain.InvoiceLine {
	return domain.InvoiceLine{
		LineNo:         no,
		Description:    hi(desc, desc),
		Quantity:       hi(qty, qty+" PCS"),
		Unit:           hi("PCS", "PCS"),
		UnitPrice:      hi(price, price),
		LineTotalMinor: hi(totalMinor, fmt.Sprintf("%d", totalMinor)),
	}
}

func packLine(no int, desc, qty string, packages, grams int64) domain.PackingLine {
	return domain.PackingLine{
		LineNo:           no,
		Description:      hi(desc, desc),
		Quantity:         hi(qty, qty+" PCS"),
		Unit:             hi("PCS", "PCS"),
		Packages:         hi(packages, fmt.Sprintf("%d CTNS", packages)),
		GrossWeightGrams: hi(grams, fmt.Sprintf("%d g", grams)),
	}
}

// chargeRow builds a high-confidence charge row, e.g. "Ocean freight 600.00".
func chargeRow(kind domain.ChargeKind, label string, amountMinor int64) domain.InvoiceCharge {
	return domain.InvoiceCharge{
		Kind:        kind,
		Label:       hi(label, label),
		AmountMinor: hi(amountMinor, fmt.Sprintf("%s %d", label, amountMinor)),
	}
}

// sampleA is the shared spec's Sample A (INV-2026-0042 / PL-2026-0042),
// which contains deliberate problems.
func sampleA() (domain.InvoiceExtraction, domain.PackingListExtraction) {
	inv := domain.InvoiceExtraction{
		InvoiceNumber: hi("INV-2026-0042", "Invoice No.: INV-2026-0042"),
		InvoiceDate:   hi("2026-09-15", "Date: 15 Sep 2026"),
		SellerName:    hi("Harbour Ceramics Co., Ltd.", "Harbour Ceramics Co., Ltd."),
		BuyerName:     hi("Southbank Homewares Pty Ltd", "Southbank Homewares Pty Ltd"),
		Currency:      hi("USD", "Currency: USD"),
		Incoterm:      hi("FOB", "FOB Ningbo"),
		LineItems: []domain.InvoiceLine{
			invLine(1, "Porcelain dinner plate, 27 cm, white glazed", "1200", "3.50", 420000),
			invLine(2, "Stoneware coffee mug, 350 ml", "800", "2.10", 168000),
			invLine(3, "Bamboo serving board, 40 x 25 cm", "300", "4.80", 144000),
		},
		TotalMinor: hi(int64(730200), "TOTAL USD 7,302.00"), // lines add up to 7,320.00
	}
	pl := domain.PackingListExtraction{
		PackingListNumber: hi("PL-2026-0042", "Packing List No.: PL-2026-0042"),
		InvoiceRef:        hi("INV-2026-0042", "Invoice Ref: INV-2026-0042"),
		LineItems: []domain.PackingLine{
			packLine(1, "Porcelain dinner plate, 27 cm", "1200", 50, 540000),
			packLine(2, "Ceramic coffee mug, 350 ml", "780", 20, 210000),
			packLine(3, "Bamboo serving board, 40 x 25 cm", "300", 12, 186000),
		},
		TotalPackages:         hi(int64(84), "TOTAL 84 CTNS"), // lines add up to 82
		TotalGrossWeightGrams: hi(int64(936000), "936.00 kg"),
	}
	return inv, pl
}

// sampleB is the shared spec's Sample B (INV-2026-0057 / PL-2026-0057),
// whose documents agree. It is sold CIF without a freight or insurance row.
func sampleB() (domain.InvoiceExtraction, domain.PackingListExtraction) {
	inv := domain.InvoiceExtraction{
		InvoiceNumber: hi("INV-2026-0057", "Invoice No.: INV-2026-0057"),
		InvoiceDate:   hi("2026-09-22", "Date: 22 Sep 2026"),
		SellerName:    hi("Blue Gum Textiles Co., Ltd.", "Blue Gum Textiles Co., Ltd."),
		BuyerName:     hi("Southbank Homewares Pty Ltd", "Southbank Homewares Pty Ltd"),
		Currency:      hi("USD", "Currency: USD"),
		Incoterm:      hi("CIF", "CIF Melbourne"),
		LineItems: []domain.InvoiceLine{
			invLine(1, "100% cotton terry bath towel, 70 x 140 cm", "2000", "2.85", 570000),
			invLine(2, "Polyester fleece throw blanket, 127 x 152 cm", "500", "4.40", 220000),
			invLine(3, "Cotton face cloth, 30 x 30 cm", "5000", "0.385", 192500), // 0.385 x 5,000 = 1,925.00
		},
		TotalMinor: hi(int64(982500), "TOTAL USD 9,825.00"),
	}
	pl := domain.PackingListExtraction{
		PackingListNumber: hi("PL-2026-0057", "Packing List No.: PL-2026-0057"),
		InvoiceRef:        hi("INV-2026-0057", "Invoice Ref: INV-2026-0057"),
		LineItems: []domain.PackingLine{
			packLine(1, "100% cotton terry bath towel, 70 x 140 cm", "2000", 40, 620000),
			packLine(2, "Polyester fleece throw blanket, 127 x 152 cm", "500", 25, 310000),
			packLine(3, "Cotton face cloth, 30 x 30 cm", "5000", 20, 260000),
		},
		TotalPackages:         hi(int64(85), "TOTAL 85 CTNS"),
		TotalGrossWeightGrams: hi(int64(1190000), "1,190.00 kg"),
	}
	return inv, pl
}

// baseline is Sample B sold FOB instead of CIF. Sample B's one finding is
// the note that its CIF invoice shows no freight or insurance amount; from
// FOB there are no findings at all, so each rule test below shows only the
// findings its own change causes.
func baseline() (domain.InvoiceExtraction, domain.PackingListExtraction) {
	inv, pl := sampleB()
	inv.Incoterm = hi("FOB", "FOB Ho Chi Minh City")
	return inv, pl
}

// compare wraps the two documents in extractions and runs Compare. It also
// checks that the test data itself passes the schema, so tests only feed
// Compare what the real pipeline could.
func compare(t *testing.T, inv domain.InvoiceExtraction, pl domain.PackingListExtraction) []domain.Finding {
	t.Helper()
	if err := inv.Validate(); err != nil {
		t.Fatalf("test invoice is invalid: %v", err)
	}
	if err := pl.Validate(); err != nil {
		t.Fatalf("test packing list is invalid: %v", err)
	}
	return compareUnvalidated(t, inv, pl)
}

// compareUnvalidated runs Compare on documents that may not pass Validate(),
// for the tests of what Compare does when a caller skipped validation.
func compareUnvalidated(t *testing.T, inv domain.InvoiceExtraction, pl domain.PackingListExtraction) []domain.Finding {
	t.Helper()
	got := Compare(
		domain.Extraction{ID: "ext_inv", DocumentID: invoiceDoc, Invoice: &inv},
		domain.Extraction{ID: "ext_pl", DocumentID: packingDoc, PackingList: &pl},
	)
	if got == nil {
		t.Fatal("Compare returned nil; want an empty slice")
	}
	for _, f := range got {
		if len(f.Refs) == 0 {
			t.Errorf("%s on line %d has no refs", f.Code, f.LineNo)
		}
	}
	return got
}

// brief is the part of a finding most tests care about.
type brief struct {
	Code     string
	Severity domain.Severity
	LineNo   int
}

func briefs(fs []domain.Finding) []brief {
	out := []brief{}
	for _, f := range fs {
		out = append(out, brief{f.Code, f.Severity, f.LineNo})
	}
	return out
}

// refStrings shows refs as "document path = value", which is short enough
// to compare in a table.
func refStrings(refs []domain.Ref) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.DocumentID+" "+r.Path+" = "+r.Value)
	}
	return out
}

const (
	errSev  = domain.SeverityError
	warnSev = domain.SeverityWarning
	infoSev = domain.SeverityInfo
)

func TestSampleA(t *testing.T) {
	inv, pl := sampleA()
	got := compare(t, inv, pl)

	// Exactly the findings the shared spec (Revision 2) expects, in output
	// order: document-level first, then by line number, then by code.
	want := []brief{
		{codeInvoiceTotalMismatch, errSev, 0},
		{codePackageTotalMismatch, warnSev, 0},
		{codeDescriptionDiffers, infoSev, 1},
		{codeDescriptionDiffers, warnSev, 2},
		{codeQuantityMismatch, errSev, 2},
	}
	if !reflect.DeepEqual(briefs(got), want) {
		t.Fatalf("findings:\n got %v\nwant %v", briefs(got), want)
	}

	wantMessages := []string{
		"Invoice total: the invoice says USD 7,302.00, but its lines add up to USD 7,320.00.",
		"Total packages: the packing list says 84 packages, but its lines add up to 82 packages.",
		`Line 1 descriptions differ: only the invoice says "white glazed".`,
		`Line 2 descriptions differ: the invoice has "stoneware" where the packing list has "ceramic". ` +
			`The differing words include material or set words (stoneware, ceramic), which can change the tariff heading.`,
		"Line 2 quantity differs: invoice 800 PCS, packing list 780 PCS.",
	}
	for i, msg := range wantMessages {
		if got[i].Message != msg {
			t.Errorf("message %d:\n got %q\nwant %q", i, got[i].Message, msg)
		}
	}

	// Refs point at both sides of the quantity mismatch, with display values.
	q := got[4]
	wantRefs := []domain.Ref{
		{DocumentID: invoiceDoc, Path: "line_items[1].quantity", Value: "800 PCS", Evidence: &domain.Evidence{Page: 1, Quote: "800 PCS"}},
		{DocumentID: packingDoc, Path: "line_items[1].quantity", Value: "780 PCS", Evidence: &domain.Evidence{Page: 1, Quote: "780 PCS"}},
	}
	if !reflect.DeepEqual(q.Refs, wantRefs) {
		t.Errorf("quantity refs:\n got %+v\nwant %+v", q.Refs, wantRefs)
	}

	// The invoice total cites the printed total and every line total.
	total := got[0]
	if len(total.Refs) != 4 || total.Refs[0].Path != "total_minor" || total.Refs[0].Value != "USD 7,302.00" ||
		total.Refs[2].Value != "USD 1,680.00" {
		t.Errorf("invoice total refs: %+v", total.Refs)
	}
}

func TestSampleB(t *testing.T) {
	inv, pl := sampleB()
	got := compare(t, inv, pl)

	// The documents agree; the only finding is the note that a CIF price
	// includes freight and insurance the invoice doesn't show separately.
	want := []brief{{codeFreightInsuranceNotShown, infoSev, 0}}
	if !reflect.DeepEqual(briefs(got), want) {
		t.Fatalf("findings:\n got %v\nwant %v", briefs(got), want)
	}
	wantMsg := "The invoice terms are CIF, so its price includes the main international transport, but no freight or insurance amount is shown. " +
		"Australia values imports on an FOB basis, so the broker may need the freight and insurance amounts from the seller."
	if got[0].Message != wantMsg {
		t.Errorf("message:\n got %q\nwant %q", got[0].Message, wantMsg)
	}
	if refs := refStrings(got[0].Refs); !slices.Equal(refs, []string{"doc_invoice incoterm = CIF"}) {
		t.Errorf("refs = %q", refs)
	}
}

// Each case changes the baseline (which has no findings) in one way and
// lists every finding that should result. Where a change would also break
// a total, the total is adjusted too, so each case shows one rule on its own.
func TestCompareRules(t *testing.T) {
	type docs struct {
		inv *domain.InvoiceExtraction
		pl  *domain.PackingListExtraction
	}
	tests := []struct {
		name   string
		mutate func(d docs)
		want   []brief
	}{
		{"consistent documents", func(docs) {}, []brief{}},

		{"invoice reference differs", func(d docs) {
			d.pl.InvoiceRef = hi("INV-2026-0058", "Invoice Ref: INV-2026-0058")
		}, []brief{{codeInvoiceRefMismatch, errSev, 0}}},
		{"invoice reference differs only in case and separators", func(d docs) {
			d.pl.InvoiceRef = hi("inv 2026/0057", "Invoice Ref: inv 2026/0057")
		}, []brief{}},

		{"line on invoice only", func(d docs) {
			d.pl.LineItems = d.pl.LineItems[:2]
			d.pl.TotalPackages = hi(int64(65), "65 CTNS")
			d.pl.TotalGrossWeightGrams = hi(int64(930000), "930.00 kg")
		}, []brief{{codeLineMissing, warnSev, 3}}},
		{"line on packing list only", func(d docs) {
			d.pl.LineItems = append(d.pl.LineItems, packLine(4, "Cotton bath mat", "100", 5, 50000))
			d.pl.TotalPackages = hi(int64(90), "90 CTNS")
			d.pl.TotalGrossWeightGrams = hi(int64(1240000), "1,240.00 kg")
		}, []brief{{codeLineMissing, warnSev, 4}}},

		// Descriptions.
		{"descriptions share few words", func(d docs) {
			d.pl.LineItems[0].Description = hi("Stainless steel cutlery set", "Stainless steel cutlery set")
		}, []brief{{codeDescriptionMismatch, warnSev, 1}}},
		{"descriptions under 50% shared are a mismatch", func(d docs) {
			// {red cotton towel} vs {blue cotton rug}: 1 shared of 5 words.
			d.inv.LineItems[0].Description = hi("Red cotton towel", "Red cotton towel")
			d.pl.LineItems[0].Description = hi("Blue cotton rug", "Blue cotton rug")
		}, []brief{{codeDescriptionMismatch, warnSev, 1}}},
		{"descriptions exactly 50% shared count as differs", func(d docs) {
			// {red cotton towel} vs {blue cotton towel}: 2 shared of 4 words.
			d.inv.LineItems[0].Description = hi("Red cotton towel", "Red cotton towel")
			d.pl.LineItems[0].Description = hi("Blue cotton towel", "Blue cotton towel")
		}, []brief{{codeDescriptionDiffers, infoSev, 1}}},
		{"descriptions differ by a word that is not a material", func(d docs) {
			d.pl.LineItems[1].Description = hi("Polyester fleece throw rug, 127 x 152 cm", "Polyester fleece throw rug, 127 x 152 cm")
		}, []brief{{codeDescriptionDiffers, infoSev, 2}}},
		{"descriptions differ by a material word", func(d docs) {
			// The invoice says polyester, the packing list acrylic.
			d.pl.LineItems[1].Description = hi("Acrylic fleece throw blanket, 127 x 152 cm", "Acrylic fleece throw blanket, 127 x 152 cm")
		}, []brief{{codeDescriptionDiffers, warnSev, 2}}},
		{"words left out on the packing list are a note", func(d docs) {
			d.pl.LineItems[0].Description = hi("Cotton terry bath towel", "Cotton terry bath towel")
		}, []brief{{codeDescriptionDiffers, infoSev, 1}}},
		{"a much shorter packing list description is not a mismatch", func(d docs) {
			// Only 1 of 9 words shared, but every packing list word is on
			// the invoice. The left-out words include "cotton", a material.
			d.pl.LineItems[0].Description = hi("Towel", "Towel")
		}, []brief{{codeDescriptionDiffers, warnSev, 1}}},
		{"extra words on the packing list are a note", func(d docs) {
			d.pl.LineItems[2].Description = hi("Cotton face cloth, 30 x 30 cm, white", "Cotton face cloth, 30 x 30 cm, white")
		}, []brief{{codeDescriptionDiffers, infoSev, 3}}},
		{"set on one side only is a warning", func(d docs) {
			d.pl.LineItems[2].Description = hi("Cotton face cloth set, 30 x 30 cm", "Cotton face cloth set, 30 x 30 cm")
		}, []brief{{codeDescriptionDiffers, warnSev, 3}}},
		{"description differs only in case and punctuation", func(d docs) {
			d.pl.LineItems[2].Description = hi("COTTON FACE CLOTH 30 X 30 CM", "COTTON FACE CLOTH 30 X 30 CM")
		}, []brief{}},

		// Units and quantities.
		{"units differ so quantities are not compared", func(d docs) {
			d.pl.LineItems[1].Unit = hi("SETS", "SETS")
			d.pl.LineItems[1].Quantity = hi("250", "250 SETS")
		}, []brief{{codeUnitMismatch, warnSev, 2}}},
		{"unit spelling variants are the same unit", func(d docs) {
			d.pl.LineItems[1].Unit = hi("pieces", "pieces")
		}, []brief{}},
		{"quantity differs", func(d docs) {
			d.pl.LineItems[2].Quantity = hi("4800", "4,800 PCS")
		}, []brief{{codeQuantityMismatch, errSev, 3}}},
		{"quantity printed with more decimal places is the same", func(d docs) {
			d.pl.LineItems[1].Quantity = hi("500.00", "500.00 PCS")
		}, []brief{}},
		{"decimal quantity differs in the last place", func(d docs) {
			d.pl.LineItems[1].Quantity = hi("500.001", "500.001 PCS")
		}, []brief{{codeQuantityMismatch, errSev, 2}}},
		{"goods sold by weight agree", func(d docs) {
			// 2,450.75 KGS x 3.20 = 7,842.40, and KGS on the invoice is the
			// same unit as KG on the packing list.
			d.inv.LineItems[2].Quantity = hi("2450.75", "2,450.75")
			d.inv.LineItems[2].Unit = hi("KGS", "KGS")
			d.inv.LineItems[2].UnitPrice = hi("3.20", "3.20")
			d.inv.LineItems[2].LineTotalMinor = hi(int64(784240), "7,842.40")
			d.inv.TotalMinor = hi(int64(1574240), "TOTAL USD 15,742.40")
			d.pl.LineItems[2].Quantity = hi("2450.75", "2,450.75")
			d.pl.LineItems[2].Unit = hi("KG", "KG")
		}, []brief{}},
		{"goods sold by weight differ", func(d docs) {
			// The KGS/KG spelling difference must not hide this error.
			d.inv.LineItems[2].Quantity = hi("2450.75", "2,450.75")
			d.inv.LineItems[2].Unit = hi("KGS", "KGS")
			d.inv.LineItems[2].UnitPrice = hi("3.20", "3.20")
			d.inv.LineItems[2].LineTotalMinor = hi(int64(784240), "7,842.40")
			d.inv.TotalMinor = hi(int64(1574240), "TOTAL USD 15,742.40")
			d.pl.LineItems[2].Quantity = hi("2450.70", "2,450.70")
			d.pl.LineItems[2].Unit = hi("KG", "KG")
		}, []brief{{codeQuantityMismatch, errSev, 3}}},

		// Line totals.
		{"line total differs from quantity x price", func(d docs) {
			d.inv.LineItems[0].LineTotalMinor = hi(int64(580000), "5,800.00")
			d.inv.TotalMinor = hi(int64(992500), "TOTAL USD 9,925.00")
		}, []brief{{codeLineTotalMismatch, errSev, 1}}},
		{"line total one cent off is tolerated", func(d docs) {
			d.inv.LineItems[2].LineTotalMinor = hi(int64(192501), "1,925.01")
			d.inv.TotalMinor = hi(int64(982501), "TOTAL USD 9,825.01")
		}, []brief{}},
		{"line total two cents off is reported", func(d docs) {
			d.inv.LineItems[2].LineTotalMinor = hi(int64(192502), "1,925.02")
			d.inv.TotalMinor = hi(int64(982502), "TOTAL USD 9,825.02")
		}, []brief{{codeLineTotalMismatch, errSev, 3}}},
		{"line total rounded half-up is accepted", func(d docs) {
			// 3 x 0.335 = 1.005, which rounds half-up to 1.01.
			d.inv.LineItems[2] = invLine(3, "Cotton face cloth, 30 x 30 cm", "3", "0.335", 101)
			d.pl.LineItems[2].Quantity = hi("3", "3 PCS")
			d.inv.TotalMinor = hi(int64(790101), "TOTAL USD 7,901.01")
		}, []brief{}},

		// Invoice total and charge rows.
		{"invoice total differs from its lines", func(d docs) {
			d.inv.TotalMinor = hi(int64(982400), "TOTAL USD 9,824.00")
		}, []brief{{codeInvoiceTotalMismatch, errSev, 0}}},
		{"freight and insurance rows are part of the total", func(d docs) {
			d.inv.Incoterm = hi("CIF", "CIF Melbourne")
			d.inv.Charges = []domain.InvoiceCharge{
				chargeRow(domain.ChargeFreight, "Ocean freight", 60000),
				chargeRow(domain.ChargeInsurance, "Insurance", 2500),
			}
			d.inv.TotalMinor = hi(int64(1045000), "TOTAL USD 10,450.00")
		}, []brief{}},
		{"discount row comes off the total", func(d docs) {
			d.inv.Charges = []domain.InvoiceCharge{chargeRow(domain.ChargeDiscount, "Discount", 18000)}
			d.inv.TotalMinor = hi(int64(964500), "TOTAL USD 9,645.00")
		}, []brief{}},
		{"other charge row adds to the total", func(d docs) {
			d.inv.Charges = []domain.InvoiceCharge{chargeRow(domain.ChargeOther, "Bank charges", 3500)}
			d.inv.TotalMinor = hi(int64(986000), "TOTAL USD 9,860.00")
		}, []brief{}},
		{"total that leaves out a charge row", func(d docs) {
			d.inv.Charges = []domain.InvoiceCharge{chargeRow(domain.ChargeFreight, "Ocean freight", 60000)}
		}, []brief{{codeInvoiceTotalMismatch, errSev, 0}}},

		// Freight and insurance note.
		{"CIF with no freight or insurance row", func(d docs) {
			d.inv.Incoterm = hi("CIF", "CIF Melbourne")
		}, []brief{{codeFreightInsuranceNotShown, infoSev, 0}}},
		{"CIF with only an insurance row", func(d docs) {
			d.inv.Incoterm = hi("CIF", "CIF Melbourne")
			d.inv.Charges = []domain.InvoiceCharge{chargeRow(domain.ChargeInsurance, "Insurance", 2500)}
			d.inv.TotalMinor = hi(int64(985000), "TOTAL USD 9,850.00")
		}, []brief{}},
		{"CFR with only an other charge row", func(d docs) {
			d.inv.Incoterm = hi("CFR", "CFR Melbourne")
			d.inv.Charges = []domain.InvoiceCharge{chargeRow(domain.ChargeOther, "Documentation fee", 5000)}
			d.inv.TotalMinor = hi(int64(987500), "TOTAL USD 9,875.00")
		}, []brief{{codeFreightInsuranceNotShown, infoSev, 0}}},
		{"no Incoterm", func(d docs) {
			d.inv.Incoterm = domain.Field[string]{}
		}, []brief{}},

		// Packing list totals.
		{"package total differs from its lines", func(d docs) {
			d.pl.TotalPackages = hi(int64(86), "86 CTNS")
		}, []brief{{codePackageTotalMismatch, warnSev, 0}}},
		{"package total not printed in the rows' kind of package", func(d docs) {
			// e.g. "TOTAL: 4 PLTS" over rows counted in cartons: the
			// extraction leaves the total null rather than compare kinds.
			d.pl.TotalPackages = domain.Field[int64]{}
		}, []brief{{codeNotChecked, infoSev, 0}}},
		{"weight total differs from its lines", func(d docs) {
			d.pl.TotalGrossWeightGrams = hi(int64(1200000), "1,200.00 kg")
		}, []brief{{codeWeightTotalMismatch, warnSev, 0}}},
		{"weight total above its lines by rounding is tolerated", func(d docs) {
			// 3 lines + 1 total, each up to half a gram off: 2 g allowed.
			d.pl.TotalGrossWeightGrams = hi(int64(1190002), "2,623.5 lb")
		}, []brief{}},
		{"weight total below its lines by rounding is tolerated", func(d docs) {
			d.pl.TotalGrossWeightGrams = hi(int64(1189998), "2,623.5 lb")
		}, []brief{}},
		{"weight total beyond rounding is reported", func(d docs) {
			d.pl.TotalGrossWeightGrams = hi(int64(1190003), "2,623.5 lb")
		}, []brief{{codeWeightTotalMismatch, warnSev, 0}}},

		// Low confidence.
		{"low confidence value is reported once though two checks use it", func(d docs) {
			// Line 2 invoice quantity feeds both the quantity and line total checks.
			d.inv.LineItems[1].Quantity = low("500", "500 PCS")
		}, []brief{{codeLowConfidence, infoSev, 2}}},
		{"low confidence document total", func(d docs) {
			d.inv.TotalMinor = low(int64(982500), "TOTAL USD 9,825.00")
		}, []brief{{codeLowConfidence, infoSev, 0}}},
		{"low confidence on both sides gives one note per value", func(d docs) {
			d.inv.LineItems[2].Unit = low("PCS", "PCS")
			d.pl.LineItems[2].Unit = low("PCS", "PCS")
		}, []brief{{codeLowConfidence, infoSev, 3}, {codeLowConfidence, infoSev, 3}}},
		{"low confidence charge amount", func(d docs) {
			fee := chargeRow(domain.ChargeOther, "Bank charges", 3500)
			fee.AmountMinor = low(int64(3500), "Bank charges 35.00")
			d.inv.Charges = []domain.InvoiceCharge{fee}
			d.inv.TotalMinor = hi(int64(986000), "TOTAL USD 9,860.00")
		}, []brief{{codeLowConfidence, infoSev, 0}}},
		{"low confidence Incoterm", func(d docs) {
			d.inv.Incoterm = low("FOB", "FOB Ho Chi Minh City")
		}, []brief{{codeLowConfidence, infoSev, 0}}},

		// Missing values.
		{"missing unit price skips the line total check", func(d docs) {
			d.inv.LineItems[2].UnitPrice = domain.Field[string]{}
		}, []brief{{codeNotChecked, infoSev, 3}}},
		{"missing invoice total", func(d docs) {
			d.inv.TotalMinor = domain.Field[int64]{}
		}, []brief{{codeNotChecked, infoSev, 0}}},
		{"missing invoice reference on packing list", func(d docs) {
			d.pl.InvoiceRef = domain.Field[string]{}
		}, []brief{{codeNotChecked, infoSev, 0}}},
		{"missing line weight skips the weight total", func(d docs) {
			d.pl.LineItems[0].GrossWeightGrams = domain.Field[int64]{}
		}, []brief{{codeNotChecked, infoSev, 0}}},
		{"low confidence value in a skipped check is not reported", func(d docs) {
			// The line total is missing, so neither the line 3 check nor the
			// invoice total check runs, and the doubtful price is never used.
			d.inv.LineItems[2].UnitPrice = low("0.385", "0.385")
			d.inv.LineItems[2].LineTotalMinor = domain.Field[int64]{}
		}, []brief{{codeNotChecked, infoSev, 0}, {codeNotChecked, infoSev, 3}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, pl := baseline()
			tc.mutate(docs{&inv, &pl})
			got := briefs(compare(t, inv, pl))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("findings:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestMessagesAndRefs(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*domain.InvoiceExtraction, *domain.PackingListExtraction)
		wantMsg  string
		wantRefs []string // "document path = value"
	}{
		{"line total", func(inv *domain.InvoiceExtraction, _ *domain.PackingListExtraction) {
			inv.LineItems[0].LineTotalMinor = hi(int64(580000), "5,800.00")
			inv.TotalMinor = hi(int64(992500), "TOTAL USD 9,925.00")
		},
			"Line 1 total is USD 5,800.00, but 2,000 PCS × USD 2.85 = USD 5,700.00.",
			[]string{
				"doc_invoice line_items[0].quantity = 2,000 PCS",
				"doc_invoice line_items[0].unit_price = USD 2.85",
				"doc_invoice line_items[0].line_total_minor = USD 5,800.00",
			}},
		{"line total with a decimal quantity", func(inv *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			inv.LineItems[2].Quantity = hi("2450.75", "2,450.75")
			inv.LineItems[2].Unit = hi("KGS", "KGS")
			inv.LineItems[2].UnitPrice = hi("3.20", "3.20")
			inv.LineItems[2].LineTotalMinor = hi(int64(784300), "7,843.00")
			inv.TotalMinor = hi(int64(1574300), "TOTAL USD 15,743.00")
			pl.LineItems[2].Quantity = hi("2450.75", "2,450.75")
			pl.LineItems[2].Unit = hi("KG", "KG")
		},
			"Line 3 total is USD 7,843.00, but 2,450.75 KGS × USD 3.20 = USD 7,842.40.",
			[]string{
				"doc_invoice line_items[2].quantity = 2,450.75 KGS",
				"doc_invoice line_items[2].unit_price = USD 3.20",
				"doc_invoice line_items[2].line_total_minor = USD 7,843.00",
			}},
		{"invoice total with charge rows names both parts", func(inv *domain.InvoiceExtraction, _ *domain.PackingListExtraction) {
			inv.Incoterm = hi("CIF", "CIF Melbourne")
			inv.Charges = []domain.InvoiceCharge{
				chargeRow(domain.ChargeFreight, "Ocean freight", 60000),
				chargeRow(domain.ChargeInsurance, "Insurance", 2500),
				chargeRow(domain.ChargeDiscount, "Discount", 10000),
			}
			// Goods 9,825.00 + 600.00 + 25.00 - 100.00 = 10,350.00.
			inv.TotalMinor = hi(int64(1045000), "TOTAL USD 10,450.00")
		},
			"Invoice total: the invoice says USD 10,450.00, but its goods lines (USD 9,825.00) plus charges less discounts (USD 525.00) add up to USD 10,350.00.",
			[]string{
				"doc_invoice total_minor = USD 10,450.00",
				"doc_invoice line_items[0].line_total_minor = USD 5,700.00",
				"doc_invoice line_items[1].line_total_minor = USD 2,200.00",
				"doc_invoice line_items[2].line_total_minor = USD 1,925.00",
				"doc_invoice charges[0].amount_minor = USD 600.00",
				"doc_invoice charges[1].amount_minor = USD 25.00",
				"doc_invoice charges[2].amount_minor = USD 100.00",
			}},
		{"freight and insurance not shown", func(inv *domain.InvoiceExtraction, _ *domain.PackingListExtraction) {
			inv.Incoterm = hi("CFR", "CFR Melbourne")
		},
			"The invoice terms are CFR, so its price includes the main international transport, but no freight or insurance amount is shown. " +
				"Australia values imports on an FOB basis, so the broker may need the freight and insurance amounts from the seller.",
			[]string{"doc_invoice incoterm = CFR"}},
		{"weight total", func(_ *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			pl.TotalGrossWeightGrams = hi(int64(1200500), "1,200.50 kg")
		},
			"Total gross weight: the packing list says 1,200.5 kg, but its lines add up to 1,190 kg.",
			[]string{
				"doc_packing total_gross_weight_grams = 1,200.5 kg",
				"doc_packing line_items[0].gross_weight_grams = 620 kg",
				"doc_packing line_items[1].gross_weight_grams = 310 kg",
				"doc_packing line_items[2].gross_weight_grams = 260 kg",
			}},
		{"description differs by a set word", func(_ *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			pl.LineItems[2].Description = hi("Cotton face cloth set, 30 x 30 cm", "Cotton face cloth set, 30 x 30 cm")
		},
			`Line 3 descriptions differ: only the packing list says "set". The differing words include material or set words (set), which can change the tariff heading.`,
			[]string{
				"doc_invoice line_items[2].description = Cotton face cloth, 30 x 30 cm",
				"doc_packing line_items[2].description = Cotton face cloth set, 30 x 30 cm",
			}},
		{"not checked names the missing value", func(inv *domain.InvoiceExtraction, _ *domain.PackingListExtraction) {
			inv.LineItems[2].UnitPrice = domain.Field[string]{}
		},
			"Line 3 total not checked: invoice line 3 unit price not found.",
			[]string{"doc_invoice line_items[2].unit_price = not found"}},
		{"low confidence", func(_ *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			pl.LineItems[1].Quantity = low("500", "500 PCS")
		},
			"The model rated the packing list line 2 quantity (500 PCS) as low confidence; check it against the document.",
			[]string{"doc_packing line_items[1].quantity = 500 PCS"}},
		{"unit mismatch", func(_ *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			pl.LineItems[1].Unit = hi("SETS", "SETS")
		},
			"Line 2 units differ: invoice PCS, packing list SETS, so quantities were not compared.",
			[]string{"doc_invoice line_items[1].unit = PCS", "doc_packing line_items[1].unit = SETS"}},
		{"invoice reference", func(_ *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			pl.InvoiceRef = hi("INV-2026-0058", "Invoice Ref: INV-2026-0058")
		},
			"The packing list refers to invoice INV-2026-0058, but the invoice number is INV-2026-0057.",
			[]string{"doc_invoice invoice_number = INV-2026-0057", "doc_packing invoice_ref = INV-2026-0058"}},
		{"line missing", func(_ *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			pl.LineItems = pl.LineItems[:2]
			pl.TotalPackages = hi(int64(65), "65 CTNS")
			pl.TotalGrossWeightGrams = hi(int64(930000), "930.00 kg")
		},
			"Line 3 is on the invoice but not on the packing list.",
			[]string{"doc_invoice line_items[2].description = Cotton face cloth, 30 x 30 cm"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, pl := baseline()
			tc.mutate(&inv, &pl)
			got := compare(t, inv, pl)
			if len(got) != 1 {
				t.Fatalf("want one finding, got %v", briefs(got))
			}
			if got[0].Message != tc.wantMsg {
				t.Errorf("message:\n got %q\nwant %q", got[0].Message, tc.wantMsg)
			}
			if refs := refStrings(got[0].Refs); !slices.Equal(refs, tc.wantRefs) {
				t.Errorf("refs:\n got %q\nwant %q", refs, tc.wantRefs)
			}
		})
	}
}

// Every spelling pair the contract lists must still let quantities be
// compared: before the table covered them, "KGS" against "KG" turned a
// quantity error into a unit_mismatch warning, and an approval no longer
// needed a note.
func TestUnitSpellingsStillCompareQuantities(t *testing.T) {
	same := [][2]string{
		{"KG", "KGS"}, {"KGS", "KILOGRAM"}, {"kilograms", "KG"},
		{"SET", "SETS"},
		{"PR", "PAIRS"}, {"PRS", "PAIR"},
		{"DZ", "DOZ"}, {"DOZEN", "DOZENS"},
		{"M", "MTRS"}, {"MTR", "METRES"}, {"METRE", "METERS"}, {"METER", "M"},
		{"ROLL", "ROLLS"}, {"BAG", "BAGS"}, {"BOX", "BOXES"}, {"UNIT", "UNITS"},
		{"PC", "PIECES"}, {"PCS", "PIECE"},
		{"CTN", "CARTONS"}, {"CTNS", "CARTON"},
	}
	different := [][2]string{
		{"PCS", "SETS"}, {"PCS", "KG"}, {"PCS", "CTNS"}, {"KG", "M"},
		{"DZ", "PCS"}, {"PR", "PCS"}, {"ROLL", "M"}, {"BOX", "CTN"},
	}
	run := func(pair [2]string, want []brief) {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			inv, pl := baseline()
			inv.LineItems[1].Unit = hi(pair[0], pair[0])
			pl.LineItems[1].Unit = hi(pair[1], pair[1])
			pl.LineItems[1].Quantity = hi("499", "499")
			if got := briefs(compare(t, inv, pl)); !reflect.DeepEqual(got, want) {
				t.Fatalf("findings:\n got %v\nwant %v", got, want)
			}
		})
	}
	for _, pair := range same {
		run(pair, []brief{{codeQuantityMismatch, errSev, 2}})
	}
	for _, pair := range different {
		run(pair, []brief{{codeUnitMismatch, warnSev, 2}})
	}
}

func TestFreightInsuranceNoteByIncoterm(t *testing.T) {
	// The seller pays the main international transport under these terms.
	withTransport := map[string]bool{"CFR": true, "CIF": true, "CPT": true, "CIP": true, "DAP": true, "DPU": true, "DDP": true}
	for _, term := range []string{"EXW", "FCA", "FAS", "FOB", "CFR", "CIF", "CPT", "CIP", "DAP", "DPU", "DDP"} {
		t.Run(term, func(t *testing.T) {
			inv, pl := baseline()
			inv.Incoterm = hi(term, term+" Melbourne")
			want := []brief{}
			if withTransport[term] {
				want = []brief{{codeFreightInsuranceNotShown, infoSev, 0}}
			}
			if got := briefs(compare(t, inv, pl)); !reflect.DeepEqual(got, want) {
				t.Fatalf("findings:\n got %v\nwant %v", got, want)
			}
		})
	}
}

// Validate() rejects each of these inputs, and the first assertion pins that
// down, because Compare relies on it: a padded unit price " 2.85" used to
// pass Validate() and quietly switch the line total check off. Compare must
// still not guess if a caller skips validation: it reports what it could
// not check instead.
func TestInputThatValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.InvoiceExtraction, *domain.PackingListExtraction)
		want    []brief
		wantMsg string // message of the first finding
	}{
		{"padded unit price", func(inv *domain.InvoiceExtraction, _ *domain.PackingListExtraction) {
			inv.LineItems[0].UnitPrice = hi(" 2.85", "2.85")
		},
			[]brief{{codeNotChecked, infoSev, 1}},
			`Line 1 total not checked: not a plain decimal number: invoice line 1 unit price " 2.85".`},
		{"padded packing list quantity", func(_ *domain.InvoiceExtraction, pl *domain.PackingListExtraction) {
			pl.LineItems[1].Quantity = hi("500 ", "500")
		},
			[]brief{{codeNotChecked, infoSev, 2}},
			`Line 2 quantity not checked: not a plain decimal number: packing list line 2 quantity "500 ".`},
		{"quantity with a thousands separator", func(inv *domain.InvoiceExtraction, _ *domain.PackingListExtraction) {
			inv.LineItems[0].Quantity = hi("2,000", "2,000")
		},
			// Both checks that read the quantity say they skipped it.
			[]brief{{codeNotChecked, infoSev, 1}, {codeNotChecked, infoSev, 1}},
			`Line 1 quantity not checked: not a plain decimal number: invoice line 1 quantity "2,000".`},
		{"charge row without an amount", func(inv *domain.InvoiceExtraction, _ *domain.PackingListExtraction) {
			inv.Charges = []domain.InvoiceCharge{{Kind: domain.ChargeFreight, Label: hi("Ocean freight", "Ocean freight")}}
		},
			[]brief{{codeNotChecked, infoSev, 0}},
			"Invoice total not checked: invoice charge row 1 (freight) amount not found."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, pl := baseline()
			tc.mutate(&inv, &pl)
			if inv.Validate() == nil && pl.Validate() == nil {
				t.Fatal("Validate() accepted the input; the checks rely on it being rejected")
			}
			got := compareUnvalidated(t, inv, pl)
			if !reflect.DeepEqual(briefs(got), tc.want) {
				t.Fatalf("findings:\n got %v\nwant %v", briefs(got), tc.want)
			}
			if got[0].Message != tc.wantMsg {
				t.Errorf("message:\n got %q\nwant %q", got[0].Message, tc.wantMsg)
			}
		})
	}
}

// The same input must give the same output, and the order lines are listed
// in must not change which findings come out.
func TestCompareIsDeterministic(t *testing.T) {
	inv, pl := sampleA()
	first := compare(t, inv, pl)
	if second := compare(t, inv, pl); !reflect.DeepEqual(first, second) {
		t.Fatal("two runs on the same input gave different findings")
	}

	slices.Reverse(pl.LineItems)
	slices.Reverse(inv.LineItems)
	if got := briefs(compare(t, inv, pl)); !reflect.DeepEqual(got, briefs(first)) {
		t.Fatalf("reordering lines changed the findings:\n got %v\nwant %v", got, briefs(first))
	}
}

func TestComparePanicsOnWrongExtractionKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic when the packing list extraction is missing")
		}
	}()
	inv, _ := sampleB()
	Compare(domain.Extraction{Invoice: &inv}, domain.Extraction{})
}
