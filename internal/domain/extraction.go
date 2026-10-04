package domain

import "fmt"

// Money rule: amounts are integers in the currency's minor unit (cents),
// never float64. 0.1 + 0.2 != 0.3 in floating point, and invoice totals
// must add up exactly.

// InvoiceExtraction is what we read from a commercial invoice.
type InvoiceExtraction struct {
	InvoiceNumber Field[string] `json:"invoice_number"`
	InvoiceDate   Field[string] `json:"invoice_date"` // YYYY-MM-DD
	SellerName    Field[string] `json:"seller_name"`
	BuyerName     Field[string] `json:"buyer_name"`
	Currency      Field[string] `json:"currency"` // ISO 4217, e.g. AUD
	Incoterm      Field[string] `json:"incoterm"` // optional
	LineItems     []InvoiceLine `json:"line_items"`
	TotalMinor    Field[int64]  `json:"total_minor"` // invoice total in cents
}

type InvoiceLine struct {
	LineNo         int           `json:"line_no"`
	Description    Field[string] `json:"description"`
	Quantity       Field[int64]  `json:"quantity"`
	Unit           Field[string] `json:"unit"` // e.g. PCS, CTN, KG
	UnitPriceMinor Field[int64]  `json:"unit_price_minor"`
	LineTotalMinor Field[int64]  `json:"line_total_minor"`
	OriginCountry  Field[string] `json:"origin_country"` // ISO 3166 alpha-2, optional
}

// Validate checks that the shape and formats are legal.
// It does NOT check whether the numbers add up: an invoice whose lines
// don't sum to its total is a real document with a real problem, and we
// report that as a Finding (day 3-4), not reject it here.
func (inv InvoiceExtraction) Validate() error {
	var errs ValidationErrors
	checkString(&errs, "invoice_number", inv.InvoiceNumber, true, nil, "")
	checkDate(&errs, "invoice_date", inv.InvoiceDate, true)
	checkString(&errs, "seller_name", inv.SellerName, true, nil, "")
	checkString(&errs, "buyer_name", inv.BuyerName, false, nil, "")
	checkString(&errs, "currency", inv.Currency, true, reCurrency, "a 3-letter ISO 4217 code such as AUD")
	checkIncoterm(&errs, "incoterm", inv.Incoterm)
	checkInt(&errs, "total_minor", inv.TotalMinor, false, 0)

	if len(inv.LineItems) == 0 {
		errs.add("line_items", "must contain at least one line")
	}
	seen := map[int]bool{}
	for i, l := range inv.LineItems {
		p := fmt.Sprintf("line_items[%d]", i)
		if l.LineNo < 1 {
			errs.add(p+".line_no", "must be >= 1")
		} else if seen[l.LineNo] {
			errs.add(p+".line_no", "duplicate line number %d", l.LineNo)
		}
		seen[l.LineNo] = true
		checkString(&errs, p+".description", l.Description, true, nil, "")
		checkInt(&errs, p+".quantity", l.Quantity, true, 1)
		checkString(&errs, p+".unit", l.Unit, true, nil, "")
		checkInt(&errs, p+".unit_price_minor", l.UnitPriceMinor, false, 0)
		checkInt(&errs, p+".line_total_minor", l.LineTotalMinor, false, 0)
		checkString(&errs, p+".origin_country", l.OriginCountry, false, reCountry, "a 2-letter ISO 3166 code such as CN")
	}
	return errs.Err()
}

// PackingListExtraction is what we read from a packing list.
type PackingListExtraction struct {
	PackingListNumber     Field[string] `json:"packing_list_number"`
	InvoiceRef            Field[string] `json:"invoice_ref"` // invoice number printed on the packing list
	LineItems             []PackingLine `json:"line_items"`
	TotalPackages         Field[int64]  `json:"total_packages"`
	TotalGrossWeightGrams Field[int64]  `json:"total_gross_weight_grams"`
}

type PackingLine struct {
	LineNo           int           `json:"line_no"`
	Description      Field[string] `json:"description"`
	Quantity         Field[int64]  `json:"quantity"`
	Unit             Field[string] `json:"unit"`
	Packages         Field[int64]  `json:"packages"`
	GrossWeightGrams Field[int64]  `json:"gross_weight_grams"` // grams, integer, same reason as money
}

func (pl PackingListExtraction) Validate() error {
	var errs ValidationErrors
	checkString(&errs, "packing_list_number", pl.PackingListNumber, false, nil, "")
	checkString(&errs, "invoice_ref", pl.InvoiceRef, false, nil, "")
	checkInt(&errs, "total_packages", pl.TotalPackages, false, 1)
	checkInt(&errs, "total_gross_weight_grams", pl.TotalGrossWeightGrams, false, 1)

	if len(pl.LineItems) == 0 {
		errs.add("line_items", "must contain at least one line")
	}
	seen := map[int]bool{}
	for i, l := range pl.LineItems {
		p := fmt.Sprintf("line_items[%d]", i)
		if l.LineNo < 1 {
			errs.add(p+".line_no", "must be >= 1")
		} else if seen[l.LineNo] {
			errs.add(p+".line_no", "duplicate line number %d", l.LineNo)
		}
		seen[l.LineNo] = true
		checkString(&errs, p+".description", l.Description, true, nil, "")
		checkInt(&errs, p+".quantity", l.Quantity, true, 1)
		checkString(&errs, p+".unit", l.Unit, true, nil, "")
		checkInt(&errs, p+".packages", l.Packages, false, 1)
		checkInt(&errs, p+".gross_weight_grams", l.GrossWeightGrams, false, 1)
	}
	return errs.Err()
}
