package claude

import (
	"strings"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// The wire types are the exact shape of the model's JSON answer, as the
// schemas in schema.go describe it. They differ from the domain types in one
// way. Structured outputs allow at most 16 nullable ("union") values in a
// schema, and an invoice has far more fields than that, so a field the
// document doesn't print can't simply be null. Instead every field carries a
// "found" flag, and toDomain turns found:false into an empty domain.Field, so
// the rest of the system still sees "not found" as a nil value.
//
// The model's answer is decoded into these types, never straight into the
// domain types. schema_test.go checks that the schemas, these types and the
// domain types all describe the same data.

type wireEvidence struct {
	Page  int    `json:"page"`
	Quote string `json:"quote"`
}

// wireField is one extracted value. When Found is false the model still has
// to fill in the other properties (the schema requires them), so they hold
// placeholders and are thrown away.
type wireField[T any] struct {
	Found      bool         `json:"found"`
	Evidence   wireEvidence `json:"evidence"`
	Value      T            `json:"value"`
	Confidence string       `json:"confidence"`
}

func (f wireField[T]) toDomain() domain.Field[T] {
	if !f.Found {
		return domain.Field[T]{}
	}
	v := f.Value
	return domain.Field[T]{
		Value:      &v,
		Confidence: domain.Confidence(enumCase(f.Confidence)),
		Evidence:   &domain.Evidence{Page: f.Evidence.Page, Quote: f.Evidence.Quote},
	}
}

// enumCase lower-cases a value from a lower-case enum. Structured outputs
// guarantee an enum value's spelling but not its capitalisation, so "High"
// is possible; it means "high", and isn't worth a paid retry.
func enumCase(s string) string { return strings.ToLower(s) }

type invoiceWire struct {
	InvoiceNumber wireField[string] `json:"invoice_number"`
	InvoiceDate   wireField[string] `json:"invoice_date"`
	SellerName    wireField[string] `json:"seller_name"`
	BuyerName     wireField[string] `json:"buyer_name"`
	Currency      wireField[string] `json:"currency"`
	Incoterm      wireField[string] `json:"incoterm"`
	LineItems     []invoiceLineWire `json:"line_items"`
	Charges       []chargeWire      `json:"charges"`
	TotalMinor    wireField[int64]  `json:"total_minor"`
}

type invoiceLineWire struct {
	LineNo         int               `json:"line_no"`
	Description    wireField[string] `json:"description"`
	Quantity       wireField[string] `json:"quantity"`
	Unit           wireField[string] `json:"unit"`
	UnitPrice      wireField[string] `json:"unit_price"`
	LineTotalMinor wireField[int64]  `json:"line_total_minor"`
	OriginCountry  wireField[string] `json:"origin_country"`
}

type chargeWire struct {
	Kind        string            `json:"kind"`
	Label       wireField[string] `json:"label"`
	AmountMinor wireField[int64]  `json:"amount_minor"`
}

func (w invoiceWire) toDomain() domain.InvoiceExtraction {
	inv := domain.InvoiceExtraction{
		InvoiceNumber: w.InvoiceNumber.toDomain(),
		InvoiceDate:   w.InvoiceDate.toDomain(),
		SellerName:    w.SellerName.toDomain(),
		BuyerName:     w.BuyerName.toDomain(),
		Currency:      w.Currency.toDomain(),
		Incoterm:      w.Incoterm.toDomain(),
		// make, not nil, so an invoice without charges stores "charges": []
		// like the hand-made extractions do.
		LineItems:  make([]domain.InvoiceLine, len(w.LineItems)),
		Charges:    make([]domain.InvoiceCharge, len(w.Charges)),
		TotalMinor: w.TotalMinor.toDomain(),
	}
	for i, l := range w.LineItems {
		inv.LineItems[i] = domain.InvoiceLine{
			LineNo:         l.LineNo,
			Description:    l.Description.toDomain(),
			Quantity:       l.Quantity.toDomain(),
			Unit:           l.Unit.toDomain(),
			UnitPrice:      l.UnitPrice.toDomain(),
			LineTotalMinor: l.LineTotalMinor.toDomain(),
			OriginCountry:  l.OriginCountry.toDomain(),
		}
	}
	for i, c := range w.Charges {
		inv.Charges[i] = domain.InvoiceCharge{
			Kind:        domain.ChargeKind(enumCase(c.Kind)),
			Label:       c.Label.toDomain(),
			AmountMinor: c.AmountMinor.toDomain(),
		}
	}
	return inv
}

type packingListWire struct {
	PackingListNumber     wireField[string] `json:"packing_list_number"`
	InvoiceRef            wireField[string] `json:"invoice_ref"`
	LineItems             []packingLineWire `json:"line_items"`
	TotalPackages         wireField[int64]  `json:"total_packages"`
	TotalGrossWeightGrams wireField[int64]  `json:"total_gross_weight_grams"`
}

type packingLineWire struct {
	LineNo           int               `json:"line_no"`
	Description      wireField[string] `json:"description"`
	Quantity         wireField[string] `json:"quantity"`
	Unit             wireField[string] `json:"unit"`
	Packages         wireField[int64]  `json:"packages"`
	GrossWeightGrams wireField[int64]  `json:"gross_weight_grams"`
}

func (w packingListWire) toDomain() domain.PackingListExtraction {
	pl := domain.PackingListExtraction{
		PackingListNumber:     w.PackingListNumber.toDomain(),
		InvoiceRef:            w.InvoiceRef.toDomain(),
		LineItems:             make([]domain.PackingLine, len(w.LineItems)),
		TotalPackages:         w.TotalPackages.toDomain(),
		TotalGrossWeightGrams: w.TotalGrossWeightGrams.toDomain(),
	}
	for i, l := range w.LineItems {
		pl.LineItems[i] = domain.PackingLine{
			LineNo:           l.LineNo,
			Description:      l.Description.toDomain(),
			Quantity:         l.Quantity.toDomain(),
			Unit:             l.Unit.toDomain(),
			Packages:         l.Packages.toDomain(),
			GrossWeightGrams: l.GrossWeightGrams.toDomain(),
		}
	}
	return pl
}
