// Package checks compares a commercial invoice with its packing list.
//
// Compare is a pure function: no I/O, no clock, no randomness. The same two
// extractions always give the same findings in the same order, so every rule
// can be unit-tested and an old check run can still be explained.
//
// Findings are for a person to read. They never change or block the data: a
// broker may know why the documents differ (a short shipment, a split
// consignment) and says so when signing off.
package checks

import (
	"cmp"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// Finding codes. The UI and the audit trail rely on them, so once released
// they never change.
const (
	codeInvoiceRefMismatch       = "invoice_ref_mismatch"
	codeLineMissing              = "line_missing"
	codeDescriptionMismatch      = "description_mismatch"
	codeDescriptionDiffers       = "description_differs"
	codeUnitMismatch             = "unit_mismatch"
	codeQuantityMismatch         = "quantity_mismatch"
	codeLineTotalMismatch        = "line_total_mismatch"
	codeInvoiceTotalMismatch     = "invoice_total_mismatch"
	codeFreightInsuranceNotShown = "freight_insurance_not_shown"
	codePackageTotalMismatch     = "package_total_mismatch"
	codeWeightTotalMismatch      = "weight_total_mismatch"
	codeLowConfidence            = "low_confidence"
	codeNotChecked               = "not_checked"
)

// Compare reports every disagreement it finds between an invoice and its
// packing list, plus a note for each check it could not run. It never
// returns nil, so "no findings" is [] in JSON rather than null.
//
// invoice.Invoice and packing.PackingList must be set; the HTTP handler
// only passes extractions of the right document type.
func Compare(invoice, packing domain.Extraction) []domain.Finding {
	if invoice.Invoice == nil || packing.PackingList == nil {
		// A bug in the caller, not bad input. Failing loudly is safer than
		// returning an empty list, which would look like "all clear".
		panic("checks.Compare: needs an invoice extraction and a packing list extraction")
	}
	c := newChecker(invoice.DocumentID, *invoice.Invoice, packing.DocumentID, *packing.PackingList)

	// Document-level checks.
	c.checkInvoiceRef()
	c.checkInvoiceTotal()
	c.checkFreightInsurance()
	var packages, weights []value
	for _, l := range c.packingLines {
		packages = append(packages, l.packages)
		weights = append(weights, l.grossWeight)
	}
	c.checkSum(codePackageTotalMismatch, domain.SeverityWarning, "Total packages", c.totalPackages, packages, formatPackages, 0)
	// Weights reach us in grams, and a weight printed in pounds has been
	// converted and rounded to the nearest gram, value by value. Each line
	// and the total can then be up to half a gram off, so a correct document
	// can be out by up to (lines+1)/2 grams. We allow ⌈(lines+1)/2⌉ grams,
	// written in integer maths as (lines+2)/2: 2 g for a three-line list.
	weightSlack := int64(len(weights)+2) / 2
	c.checkSum(codeWeightTotalMismatch, domain.SeverityWarning, "Total gross weight", c.totalWeight, weights, formatWeight, weightSlack)

	// Line checks. Lines are paired by their line number, not by their
	// position in the list, so a line missing from one document doesn't
	// shift the rest.
	invoiceByNo := map[int]invoiceLine{}
	for _, l := range c.invoiceLines {
		invoiceByNo[l.no] = l
	}
	packingByNo := map[int]packingLine{}
	for _, l := range c.packingLines {
		packingByNo[l.no] = l
	}
	allNos := map[int]bool{}
	for n := range invoiceByNo {
		allNos[n] = true
	}
	for n := range packingByNo {
		allNos[n] = true
	}
	for _, n := range slices.Sorted(maps.Keys(allNos)) {
		il, onInvoice := invoiceByNo[n]
		pl, onPacking := packingByNo[n]
		switch {
		case onInvoice && onPacking:
			c.checkDescriptions(il, pl)
			c.checkQuantities(il, pl)
		case onInvoice:
			c.add(codeLineMissing, domain.SeverityWarning, n,
				fmt.Sprintf("Line %d is on the invoice but not on the packing list.", n),
				[]domain.Ref{il.description.ref})
		default:
			c.add(codeLineMissing, domain.SeverityWarning, n,
				fmt.Sprintf("Line %d is on the packing list but not on the invoice.", n),
				[]domain.Ref{pl.description.ref})
		}
		// Quantity x price only needs the invoice, so it runs even when
		// the packing list has no matching line.
		if onInvoice {
			c.checkLineTotal(il)
		}
	}

	// Document-level findings (line 0) first, then by line number, then by
	// code. The sort is stable, so findings with the same line and code
	// (e.g. two low_confidence notes) keep the fixed order the checks above
	// produced them in.
	slices.SortStableFunc(c.findings, func(a, b domain.Finding) int {
		return cmp.Or(cmp.Compare(a.LineNo, b.LineNo), cmp.Compare(a.Code, b.Code))
	})
	return c.findings
}

// ---- the values being compared ----

// source is one of the two documents being compared.
type source struct {
	name string // "invoice" or "packing list", used in messages
	id   string // document ID, used in Refs
}

// value is one extracted value a check reads, with everything needed to
// cite it in a finding: where it is, how to show it and how sure the model
// was about it.
type value struct {
	doc        string // "invoice" or "packing list"
	label      string // e.g. "invoice line 2 quantity", used in messages
	lineNo     int    // 0 for document-level values such as totals
	present    bool   // false when the document didn't have it (null)
	confidence domain.Confidence
	ref        domain.Ref
	text       string // raw value of a string field, e.g. a quantity "1250.5"
	num        int64  // raw value of a number field
}

func newValue[T any](src source, path, label string, lineNo int, f domain.Field[T], show func(T) string) value {
	v := value{
		doc:        src.name,
		label:      src.name + " " + label,
		lineNo:     lineNo,
		present:    f.Present(),
		confidence: f.Confidence,
		ref:        domain.Ref{DocumentID: src.id, Path: path, Value: "not found", Evidence: f.Evidence},
	}
	if f.Present() {
		v.ref.Value = show(*f.Value)
	}
	return v
}

func textValue(src source, path, label string, lineNo int, f domain.Field[string], show func(string) string) value {
	v := newValue(src, path, label, lineNo, f, show)
	if f.Present() {
		v.text = *f.Value
	}
	return v
}

func numValue(src source, path, label string, lineNo int, f domain.Field[int64], show func(*big.Int) string) value {
	v := newValue(src, path, label, lineNo, f, func(n int64) string { return show(big.NewInt(n)) })
	if f.Present() {
		v.num = *f.Value
	}
	return v
}

func asIs(s string) string { return s }

// invoiceLine and packingLine hold one line's values, ready to compare.
type invoiceLine struct {
	no                                                int
	description, quantity, unit, unitPrice, lineTotal value
}

type packingLine struct {
	no                                                 int
	description, quantity, unit, packages, grossWeight value
}

// charge is one non-goods row of the invoice, such as freight or a discount.
type charge struct {
	kind   domain.ChargeKind
	amount value
}

func newInvoiceLine(src source, idx int, l domain.InvoiceLine, cur currency) invoiceLine {
	// Paths use the position in the JSON array (what the UI can look up);
	// labels use the line number (what a person sees).
	path := func(field string) string { return fmt.Sprintf("line_items[%d].%s", idx, field) }
	label := func(what string) string { return fmt.Sprintf("line %d %s", l.LineNo, what) }
	unit := ""
	if l.Unit.Present() {
		unit = *l.Unit.Value
	}
	return invoiceLine{
		no:          l.LineNo,
		description: textValue(src, path("description"), label("description"), l.LineNo, l.Description, asIs),
		quantity:    textValue(src, path("quantity"), label("quantity"), l.LineNo, l.Quantity, func(q string) string { return formatCount(q, unit) }),
		unit:        textValue(src, path("unit"), label("unit"), l.LineNo, l.Unit, asIs),
		unitPrice:   textValue(src, path("unit_price"), label("unit price"), l.LineNo, l.UnitPrice, cur.price),
		lineTotal:   numValue(src, path("line_total_minor"), label("total"), l.LineNo, l.LineTotalMinor, cur.amount),
	}
}

func newPackingLine(src source, idx int, l domain.PackingLine) packingLine {
	path := func(field string) string { return fmt.Sprintf("line_items[%d].%s", idx, field) }
	label := func(what string) string { return fmt.Sprintf("line %d %s", l.LineNo, what) }
	unit := ""
	if l.Unit.Present() {
		unit = *l.Unit.Value
	}
	return packingLine{
		no:          l.LineNo,
		description: textValue(src, path("description"), label("description"), l.LineNo, l.Description, asIs),
		quantity:    textValue(src, path("quantity"), label("quantity"), l.LineNo, l.Quantity, func(q string) string { return formatCount(q, unit) }),
		unit:        textValue(src, path("unit"), label("unit"), l.LineNo, l.Unit, asIs),
		packages:    numValue(src, path("packages"), label("packages"), l.LineNo, l.Packages, formatPackages),
		grossWeight: numValue(src, path("gross_weight_grams"), label("gross weight"), l.LineNo, l.GrossWeightGrams, formatWeight),
	}
}

// ---- the checker ----

// checker collects findings while the checks run.
type checker struct {
	cur currency // the invoice currency, used to show all money

	invoiceNumber, invoiceTotal, incoterm  value
	invoiceRef, totalPackages, totalWeight value
	invoiceLines                           []invoiceLine // sorted by line number
	charges                                []charge      // in printed order
	packingLines                           []packingLine // sorted by line number

	findings []domain.Finding
	lowSeen  map[string]bool // values already reported as low confidence
}

func newChecker(invoiceID string, inv domain.InvoiceExtraction, packingID string, pl domain.PackingListExtraction) *checker {
	cur := currencyOf(inv.Currency)
	is := source{name: "invoice", id: invoiceID}
	ps := source{name: "packing list", id: packingID}
	c := &checker{
		cur:           cur,
		invoiceNumber: textValue(is, "invoice_number", "number", 0, inv.InvoiceNumber, asIs),
		invoiceTotal:  numValue(is, "total_minor", "total", 0, inv.TotalMinor, cur.amount),
		incoterm:      textValue(is, "incoterm", "Incoterm", 0, inv.Incoterm, asIs),
		invoiceRef:    textValue(ps, "invoice_ref", "invoice reference", 0, pl.InvoiceRef, asIs),
		totalPackages: numValue(ps, "total_packages", "total packages", 0, pl.TotalPackages, formatPackages),
		totalWeight:   numValue(ps, "total_gross_weight_grams", "total gross weight", 0, pl.TotalGrossWeightGrams, formatWeight),
		findings:      []domain.Finding{},
		lowSeen:       map[string]bool{},
	}
	for i, l := range inv.LineItems {
		c.invoiceLines = append(c.invoiceLines, newInvoiceLine(is, i, l, cur))
	}
	for i, ch := range inv.Charges {
		// The label names the row by position and kind rather than by its
		// printed label, which may be missing on unvalidated input.
		label := fmt.Sprintf("charge row %d (%s) amount", i+1, ch.Kind)
		c.charges = append(c.charges, charge{
			kind:   ch.Kind,
			amount: numValue(is, fmt.Sprintf("charges[%d].amount_minor", i), label, 0, ch.AmountMinor, cur.amount),
		})
	}
	for i, l := range pl.LineItems {
		c.packingLines = append(c.packingLines, newPackingLine(ps, i, l))
	}
	// Documents normally list lines in order, but the model might not;
	// sorting keeps Refs and messages in a predictable order.
	slices.SortStableFunc(c.invoiceLines, func(a, b invoiceLine) int { return cmp.Compare(a.no, b.no) })
	slices.SortStableFunc(c.packingLines, func(a, b packingLine) int { return cmp.Compare(a.no, b.no) })
	return c
}

func (c *checker) add(code string, sev domain.Severity, lineNo int, msg string, refs []domain.Ref) {
	c.findings = append(c.findings, domain.Finding{Code: code, Severity: sev, Message: msg, LineNo: lineNo, Refs: refs})
}

// requireAll returns true if every value is present. Otherwise it records a
// not_checked finding naming the missing values and returns false, so a
// check can start with `if !c.requireAll(...) { return }`.
func (c *checker) requireAll(lineNo int, check string, vals ...value) bool {
	var labels []string
	var refs []domain.Ref
	for _, v := range vals {
		if !v.present {
			labels = append(labels, v.label)
			refs = append(refs, v.ref)
		}
	}
	if len(labels) == 0 {
		return true
	}
	c.add(codeNotChecked, domain.SeverityInfo, lineNo,
		fmt.Sprintf("%s not checked: %s not found.", check, strings.Join(labels, ", ")), refs)
	return false
}

// notDecimal records that a check was skipped because a quantity or unit
// price among vals is not a plain decimal number, naming the ones that
// aren't. Validate() rejects such values (it matches the stored value, so
// even a padded " 3.50" fails), so this only happens when a caller skipped
// validation. Saying so is safer than guessing what the number was meant
// to be, or quietly skipping the check.
func (c *checker) notDecimal(lineNo int, check string, vals ...value) {
	var bad []string
	var refs []domain.Ref
	for _, v := range vals {
		if _, ok := parseDecimal(v.text); !ok {
			bad = append(bad, fmt.Sprintf("%s %q", v.label, v.text))
			refs = append(refs, v.ref)
		}
	}
	c.add(codeNotChecked, domain.SeverityInfo, lineNo,
		fmt.Sprintf("%s not checked: not a plain decimal number: %s.", check, strings.Join(bad, ", ")), refs)
}

// use returns Refs for the values a check relied on. The first time a check
// relies on a value the model rated low confidence, it also adds a
// low_confidence note for that value: a "match" built on a doubtful reading
// is worth a second look. Only checks that actually ran call use, so values
// nobody compared don't produce noise.
func (c *checker) use(vals ...value) []domain.Ref {
	refs := make([]domain.Ref, 0, len(vals))
	for _, v := range vals {
		refs = append(refs, v.ref)
		// Both documents have e.g. "line_items[0].quantity", so the key
		// needs the document name as well as the path.
		key := v.doc + " " + v.ref.Path
		if v.confidence != domain.ConfidenceLow || c.lowSeen[key] {
			continue
		}
		c.lowSeen[key] = true
		c.add(codeLowConfidence, domain.SeverityInfo, v.lineNo,
			fmt.Sprintf("The model rated the %s (%s) as low confidence; check it against the document.", v.label, v.ref.Value),
			[]domain.Ref{v.ref})
	}
	return refs
}

// sum adds number values with math/big, so a garbled or hostile document
// with huge numbers can't overflow int64 and wrap around to a wrong answer.
func sum(vals []value) *big.Int {
	total := new(big.Int)
	for _, v := range vals {
		total.Add(total, big.NewInt(v.num))
	}
	return total
}

// ---- the checks ----

func (c *checker) checkInvoiceRef() {
	if !c.requireAll(0, "Invoice reference", c.invoiceNumber, c.invoiceRef) {
		return
	}
	refs := c.use(c.invoiceNumber, c.invoiceRef)
	if normaliseRef(c.invoiceNumber.text) != normaliseRef(c.invoiceRef.text) {
		c.add(codeInvoiceRefMismatch, domain.SeverityError, 0,
			fmt.Sprintf("The packing list refers to invoice %s, but the invoice number is %s.", c.invoiceRef.text, c.invoiceNumber.text),
			refs)
	}
}

// checkInvoiceTotal compares the printed invoice total with what the
// invoice itself lists: the goods lines, plus the freight, insurance and
// other charge rows, less discounts. The printed total normally includes
// those rows, so leaving them out would flag every invoice that itemises
// freight as wrong.
func (c *checker) checkInvoiceTotal() {
	const what = "Invoice total"
	var lineTotals, chargeAmounts []value
	for _, l := range c.invoiceLines {
		lineTotals = append(lineTotals, l.lineTotal)
	}
	for _, ch := range c.charges {
		chargeAmounts = append(chargeAmounts, ch.amount)
	}
	vals := slices.Concat([]value{c.invoiceTotal}, lineTotals, chargeAmounts)
	if !c.requireAll(0, what, vals...) {
		return
	}
	refs := c.use(vals...)

	goods := sum(lineTotals)
	// Charge amounts are never negative (Validate() enforces >= 0); the kind
	// says whether a row adds to the total or comes off it. Validate() only
	// allows freight, insurance, discount and other, and every kind but a
	// discount adds.
	charges := new(big.Int)
	for _, ch := range c.charges {
		if ch.kind == domain.ChargeDiscount {
			charges.Sub(charges, big.NewInt(ch.amount.num))
		} else {
			charges.Add(charges, big.NewInt(ch.amount.num))
		}
	}
	listed := new(big.Int).Add(goods, charges)
	if listed.Cmp(big.NewInt(c.invoiceTotal.num)) == 0 {
		return
	}

	msg := fmt.Sprintf("%s: the invoice says %s, but its lines add up to %s.",
		what, c.invoiceTotal.ref.Value, c.cur.amount(goods))
	if len(c.charges) > 0 {
		// Name both parts, so a reviewer can see whether the goods lines or
		// the charge rows are out.
		msg = fmt.Sprintf("%s: the invoice says %s, but its goods lines (%s) plus charges less discounts (%s) add up to %s.",
			what, c.invoiceTotal.ref.Value, c.cur.amount(goods), c.cur.amount(charges), c.cur.amount(listed))
	}
	c.add(codeInvoiceTotalMismatch, domain.SeverityError, 0, msg, refs)
}

// termsWithTransport are the Incoterms 2020 rules under which the seller
// pays for the main international transport, so the invoice price includes
// it. (CIF and CIP also include insurance.)
var termsWithTransport = map[string]bool{
	"CFR": true, "CIF": true, "CPT": true, "CIP": true, "DAP": true, "DPU": true, "DDP": true,
}

// checkFreightInsurance notes when the terms of sale include transport but
// the invoice prints no freight or insurance row. Nothing is wrong with such
// an invoice, but Australia values imports on an FOB basis, so the broker
// has to take those costs out of the price and needs the amounts to do it.
func (c *checker) checkFreightInsurance() {
	// The Incoterm is optional. Without one nothing says the price includes
	// transport, so there is nothing to note (and no check was skipped).
	if !c.incoterm.present {
		return
	}
	refs := c.use(c.incoterm)
	// Validate() only accepts the upper-case Incoterms 2020 codes, so the
	// value can be looked up as stored.
	if !termsWithTransport[c.incoterm.text] {
		return
	}
	for _, ch := range c.charges {
		if ch.kind == domain.ChargeFreight || ch.kind == domain.ChargeInsurance {
			return
		}
	}
	c.add(codeFreightInsuranceNotShown, domain.SeverityInfo, 0,
		fmt.Sprintf("The invoice terms are %s, so its price includes the main international transport, but no freight or insurance amount is shown. "+
			"Australia values imports on an FOB basis, so the broker may need the freight and insurance amounts from the seller.", c.incoterm.text),
		refs)
}

// checkSum compares a printed packing list total with the sum of the line
// values that should make it up, allowing a difference of up to slack.
func (c *checker) checkSum(code string, sev domain.Severity, what string, total value, lines []value, show func(*big.Int) string, slack int64) {
	vals := append([]value{total}, lines...)
	if !c.requireAll(0, what, vals...) {
		return
	}
	refs := c.use(vals...)
	got := sum(lines)
	diff := new(big.Int).Sub(got, big.NewInt(total.num))
	if diff.CmpAbs(big.NewInt(slack)) > 0 {
		c.add(code, sev, 0,
			fmt.Sprintf("%s: the %s says %s, but its lines add up to %s.", what, total.doc, total.ref.Value, show(got)),
			refs)
	}
}

// checkLineTotal recomputes quantity x unit price for one invoice line.
func (c *checker) checkLineTotal(l invoiceLine) {
	what := fmt.Sprintf("Line %d total", l.no)
	if !c.requireAll(l.no, what, l.quantity, l.unitPrice, l.lineTotal) {
		return
	}
	want, ok := lineAmount(l.quantity.text, l.unitPrice.text, c.cur.decimals)
	if !ok {
		c.notDecimal(l.no, what, l.quantity, l.unitPrice)
		return
	}
	refs := c.use(l.quantity, l.unitPrice, l.lineTotal)
	// Allow 1 minor unit either way: sellers' systems round differently
	// (half-up, half-even, or from a longer price than they print), and a
	// one-cent rounding difference is not a discrepancy worth an error.
	diff := new(big.Int).Sub(want, big.NewInt(l.lineTotal.num))
	if diff.CmpAbs(big.NewInt(1)) > 0 {
		c.add(codeLineTotalMismatch, domain.SeverityError, l.no,
			fmt.Sprintf("Line %d total is %s, but %s × %s = %s.", l.no, l.lineTotal.ref.Value, l.quantity.ref.Value, l.unitPrice.ref.Value, c.cur.amount(want)),
			refs)
	}
}

// checkDescriptions compares the wording of a paired line. It can't tell
// whether two descriptions mean the same goods; it only points out where
// the words differ so a person can judge.
func (c *checker) checkDescriptions(il invoiceLine, pl packingLine) {
	n := il.no
	if !c.requireAll(n, fmt.Sprintf("Line %d description", n), il.description, pl.description) {
		return
	}
	refs := c.use(il.description, pl.description)
	invWords, plWords := words(il.description.text), words(pl.description.text)
	onlyInvoice := missingFrom(invWords, plWords)
	onlyPacking := missingFrom(plWords, invWords)
	shared := len(invWords) - len(onlyInvoice)
	union := shared + len(onlyInvoice) + len(onlyPacking)

	if len(onlyInvoice) == 0 && len(onlyPacking) == 0 {
		// Same words: only case, punctuation or order differ.
		return
	}
	// When every word of one description is in the other, it is a shortened
	// description of the same goods, however short ("Towel" for "100% cotton
	// terry bath towel, 70 x 140 cm"), not a sign of misaligned lines. Its
	// left-out words are still reported below.
	shortened := shared > 0 && (len(onlyInvoice) == 0 || len(onlyPacking) == 0)
	if !shortened && 2*shared < union {
		// Jaccard similarity shared/union below 50%, written without
		// division. So few words in common suggests the lines are not the
		// same goods, e.g. the packing list lines are in a different order.
		c.add(codeDescriptionMismatch, domain.SeverityWarning, n,
			fmt.Sprintf("Line %d descriptions have few words in common, so the lines may not be the same goods: invoice \"%s\", packing list \"%s\".", n, il.description.text, pl.description.text),
			refs)
		return
	}

	// Mostly the same words, but some appear on one side only. That is
	// reported even when the packing list just leaves words out ("Porcelain
	// dinner plate, 27 cm" for "... 27 cm, white glazed"): the reviewer
	// decides whether the missing words matter, and some can.
	var msg string
	switch {
	case len(onlyPacking) == 0:
		msg = fmt.Sprintf("Line %d descriptions differ: only the invoice says \"%s\".", n, strings.Join(onlyInvoice, " "))
	case len(onlyInvoice) == 0:
		msg = fmt.Sprintf("Line %d descriptions differ: only the packing list says \"%s\".", n, strings.Join(onlyPacking, " "))
	default:
		msg = fmt.Sprintf("Line %d descriptions differ: the invoice has \"%s\" where the packing list has \"%s\".", n, strings.Join(onlyInvoice, " "), strings.Join(onlyPacking, " "))
	}
	sev := domain.SeverityInfo
	if m := materialIn(onlyInvoice, onlyPacking); len(m) > 0 {
		// A different material, or a set instead of single items, can put
		// the goods under a different tariff heading.
		sev = domain.SeverityWarning
		msg += fmt.Sprintf(" The differing words include material or set words (%s), which can change the tariff heading.", strings.Join(m, ", "))
	}
	c.add(codeDescriptionDiffers, sev, n, msg, refs)
}

// checkQuantities compares units, then quantities if the units agree.
func (c *checker) checkQuantities(il invoiceLine, pl packingLine) {
	n := il.no
	what := fmt.Sprintf("Line %d quantity", n)
	if !c.requireAll(n, what, il.unit, pl.unit) {
		return
	}
	unitRefs := c.use(il.unit, pl.unit)
	if normaliseUnit(il.unit.text) != normaliseUnit(pl.unit.text) {
		// 800 PCS and 40 SETS can both be right; we have no way to convert,
		// so we say so instead of reporting a false quantity error.
		c.add(codeUnitMismatch, domain.SeverityWarning, n,
			fmt.Sprintf("Line %d units differ: invoice %s, packing list %s, so quantities were not compared.", n, il.unit.text, pl.unit.text),
			unitRefs)
		return
	}
	if !c.requireAll(n, what, il.quantity, pl.quantity) {
		return
	}
	// Quantities are compared as exact decimals, so "1200" and "1200.0" are
	// the same and 1,250.5 KG against 1,250.4 KG is a difference.
	same, ok := sameQuantity(il.quantity.text, pl.quantity.text)
	if !ok {
		c.notDecimal(n, what, il.quantity, pl.quantity)
		return
	}
	refs := c.use(il.quantity, pl.quantity)
	if !same {
		c.add(codeQuantityMismatch, domain.SeverityError, n,
			fmt.Sprintf("Line %d quantity differs: invoice %s, packing list %s.", n, il.quantity.ref.Value, pl.quantity.ref.Value),
			refs)
	}
}
