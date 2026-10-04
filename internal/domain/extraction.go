package domain

import "fmt"

// Money rule: amounts are integers in the currency's minor unit (cents),
// never float64. 0.1 + 0.2 != 0.3 in floating point, and invoice totals
// must add up exactly.
//
// Unit prices and quantities are the exception: prices are often quoted to a
// fraction of a cent (e.g. USD 0.385 per piece) and goods sold by weight or
// length have fractional quantities (e.g. 1,250.5 KG), so both are kept as
// the decimal string printed on the document ("0.385", "1250.5") and only
// multiplied out with exact integer maths.

// InvoiceExtraction is what we read from a commercial invoice.
type InvoiceExtraction struct {
	InvoiceNumber Field[string]   `json:"invoice_number"`
	InvoiceDate   Field[string]   `json:"invoice_date"` // YYYY-MM-DD
	SellerName    Field[string]   `json:"seller_name"`
	BuyerName     Field[string]   `json:"buyer_name"`
	Currency      Field[string]   `json:"currency"` // ISO 4217, e.g. AUD; null if only a symbol like "$" is printed
	Incoterm      Field[string]   `json:"incoterm"` // optional
	LineItems     []InvoiceLine   `json:"line_items"`
	Charges       []InvoiceCharge `json:"charges"`     // freight, insurance, discounts... printed outside the goods lines
	TotalMinor    Field[int64]    `json:"total_minor"` // invoice total in cents
}

// InvoiceCharge is a non-goods row between the goods lines and the total,
// such as "Ocean freight 450.00" or "Discount 3% -180.00". The invoice total
// usually includes them, and a broker needs freight and insurance separately
// because Australia values imports on an FOB basis.
type InvoiceCharge struct {
	Kind        ChargeKind    `json:"kind"`
	Label       Field[string] `json:"label"`        // as printed, e.g. "Ocean freight"
	AmountMinor Field[int64]  `json:"amount_minor"` // always positive; a discount is subtracted
}

type ChargeKind string

const (
	ChargeFreight   ChargeKind = "freight"
	ChargeInsurance ChargeKind = "insurance"
	ChargeDiscount  ChargeKind = "discount"
	ChargeOther     ChargeKind = "other"
)

func (k ChargeKind) Valid() bool {
	switch k {
	case ChargeFreight, ChargeInsurance, ChargeDiscount, ChargeOther:
		return true
	}
	return false
}

type InvoiceLine struct {
	LineNo         int           `json:"line_no"`
	Description    Field[string] `json:"description"`
	Quantity       Field[string] `json:"quantity"`   // decimal string as printed, e.g. "1200" or "1250.5"
	Unit           Field[string] `json:"unit"`       // e.g. PCS, CTN, KG
	UnitPrice      Field[string] `json:"unit_price"` // decimal string as printed, e.g. "3.50" or "0.385"
	LineTotalMinor Field[int64]  `json:"line_total_minor"`
	OriginCountry  Field[string] `json:"origin_country"` // ISO 3166 alpha-2, optional
}

// Validate checks that the shape and formats are legal.
// It does NOT check whether the numbers add up: an invoice whose lines
// don't sum to its total is a real document with a real problem, and we
// report that as a Finding, not reject it here.
func (inv InvoiceExtraction) Validate() error {
	var errs ValidationErrors
	checkString(&errs, "invoice_number", inv.InvoiceNumber, true, nil, "")
	checkDate(&errs, "invoice_date", inv.InvoiceDate, true)
	checkString(&errs, "seller_name", inv.SellerName, true, nil, "")
	checkString(&errs, "buyer_name", inv.BuyerName, false, nil, "")
	checkString(&errs, "currency", inv.Currency, false, reCurrency, "a 3-letter ISO 4217 code such as AUD")
	checkIncoterm(&errs, "incoterm", inv.Incoterm)
	checkInt(&errs, "total_minor", inv.TotalMinor, false, 0)

	checkLineCount(&errs, len(inv.LineItems))
	if len(inv.Charges) > maxCharges {
		errs.add("charges", "must have at most %d rows", maxCharges)
	}
	for i, c := range inv.Charges {
		p := fmt.Sprintf("charges[%d]", i)
		if !c.Kind.Valid() {
			errs.add(p+".kind", "must be one of freight, insurance, discount, other")
		}
		checkString(&errs, p+".label", c.Label, true, nil, "")
		checkInt(&errs, p+".amount_minor", c.AmountMinor, true, 0)
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
		checkQuantity(&errs, p+".quantity", l.Quantity)
		checkString(&errs, p+".unit", l.Unit, true, nil, "")
		checkString(&errs, p+".unit_price", l.UnitPrice, false, reDecimal, "a decimal number such as 3.50 (no currency symbol or thousands separator)")
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
	Quantity         Field[string] `json:"quantity"` // decimal string as printed, like InvoiceLine.Quantity
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

	checkLineCount(&errs, len(pl.LineItems))
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
		checkQuantity(&errs, p+".quantity", l.Quantity)
		checkString(&errs, p+".unit", l.Unit, true, nil, "")
		checkInt(&errs, p+".packages", l.Packages, false, 1)
		checkInt(&errs, p+".gross_weight_grams", l.GrossWeightGrams, false, 1)
	}
	return errs.Err()
}
