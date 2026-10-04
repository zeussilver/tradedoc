package domain

import (
	"errors"
	"strings"
	"testing"
)

func validInvoice() InvoiceExtraction {
	return InvoiceExtraction{
		InvoiceNumber: Of("INV-1", ConfidenceHigh, 1, "Invoice No: INV-1"),
		InvoiceDate:   Of("2026-09-15", ConfidenceHigh, 1, "Date: 15 Sep 2026"),
		SellerName:    Of("Seller Co", ConfidenceHigh, 1, "Seller: Seller Co"),
		Currency:      Of("AUD", ConfidenceHigh, 1, "AUD"),
		LineItems: []InvoiceLine{{
			LineNo:      1,
			Description: Of("Mug", ConfidenceHigh, 1, "Mug"),
			Quantity:    Of("10", ConfidenceHigh, 1, "10 PCS"),
			Unit:        Of("PCS", ConfidenceHigh, 1, "10 PCS"),
		}},
	}
}

// Table-driven test: each case changes one thing in a valid invoice and
// states which path should be reported.
func TestInvoiceValidate(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*InvoiceExtraction)
		wantPath string // "" means valid
	}{
		{"valid", func(*InvoiceExtraction) {}, ""},
		{"missing invoice number", func(i *InvoiceExtraction) { i.InvoiceNumber = Field[string]{} }, "invoice_number"},
		{"bad date format", func(i *InvoiceExtraction) { i.InvoiceDate = Of("15/09/2026", ConfidenceHigh, 1, "15/09/2026") }, "invoice_date"},
		{"lowercase currency", func(i *InvoiceExtraction) { i.Currency = Of("aud", ConfidenceHigh, 1, "aud") }, "currency"},
		{"unknown incoterm", func(i *InvoiceExtraction) { i.Incoterm = Of("XYZ", ConfidenceLow, 1, "XYZ") }, "incoterm"},
		{"value without evidence", func(i *InvoiceExtraction) {
			v := "Buyer"
			i.BuyerName = Field[string]{Value: &v, Confidence: ConfidenceHigh}
		}, "buyer_name.evidence"},
		{"bad confidence", func(i *InvoiceExtraction) { i.SellerName.Confidence = "92%" }, "seller_name.confidence"},
		{"blank string instead of null", func(i *InvoiceExtraction) { i.SellerName = Of("  ", ConfidenceHigh, 1, "x") }, "seller_name"},
		{"no lines", func(i *InvoiceExtraction) { i.LineItems = nil }, "line_items"},
		{"zero quantity", func(i *InvoiceExtraction) {
			i.LineItems[0].Quantity = Of("0.00", ConfidenceHigh, 1, "0.00")
		}, "line_items[0].quantity"},
		{"duplicate line number", func(i *InvoiceExtraction) {
			i.LineItems = append(i.LineItems, i.LineItems[0])
		}, "line_items[1].line_no"},
		{"fractional quantity is fine", func(i *InvoiceExtraction) {
			i.LineItems[0].Quantity = Of("1250.5", ConfidenceHigh, 1, "1,250.5 KG")
		}, ""},
		{"quantity with thousands separator", func(i *InvoiceExtraction) {
			i.LineItems[0].Quantity = Of("1,200", ConfidenceHigh, 1, "1,200")
		}, "line_items[0].quantity"},
		{"padded unit price is rejected, not trimmed", func(i *InvoiceExtraction) {
			i.LineItems[0].UnitPrice = Of(" 3.50", ConfidenceHigh, 1, "3.50")
		}, "line_items[0].unit_price"},
		{"quote limit counts characters, not bytes", func(i *InvoiceExtraction) {
			i.SellerName = Of("宁波", ConfidenceHigh, 1, strings.Repeat("瓷", 300))
		}, ""},
		{"quote over 300 characters", func(i *InvoiceExtraction) {
			i.SellerName = Of("宁波", ConfidenceHigh, 1, strings.Repeat("瓷", 301))
		}, "seller_name.evidence.quote"},
		{"charge with unknown kind", func(i *InvoiceExtraction) {
			i.Charges = []InvoiceCharge{{Kind: "tax", Label: Of("GST", ConfidenceHigh, 1, "GST"), AmountMinor: Of(int64(100), ConfidenceHigh, 1, "1.00")}}
		}, "charges[0].kind"},
		{"freight charge is fine", func(i *InvoiceExtraction) {
			i.Charges = []InvoiceCharge{{Kind: ChargeFreight, Label: Of("Ocean freight", ConfidenceHigh, 1, "Ocean freight"), AmountMinor: Of(int64(45000), ConfidenceHigh, 1, "450.00")}}
		}, ""},
		{"missing currency is allowed (a bare $ is not a currency)", func(i *InvoiceExtraction) { i.Currency = Field[string]{} }, ""},
		{"missing optional fields is fine", func(i *InvoiceExtraction) {
			i.BuyerName, i.Incoterm, i.TotalMinor = Field[string]{}, Field[string]{}, Field[int64]{}
		}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := validInvoice()
			tc.mutate(&inv)
			err := inv.Validate()

			if tc.wantPath == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			var ve ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("expected ValidationErrors, got %v", err)
			}
			for _, fe := range ve {
				if fe.Path == tc.wantPath {
					return
				}
			}
			t.Fatalf("expected an error at %q, got %v", tc.wantPath, ve)
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	var inv InvoiceExtraction // completely empty
	var ve ValidationErrors
	if !errors.As(inv.Validate(), &ve) {
		t.Fatal("expected ValidationErrors")
	}
	if len(ve) < 4 {
		t.Fatalf("expected every problem listed, got %d: %v", len(ve), ve)
	}
}

func TestClassificationSuggestionHSCode(t *testing.T) {
	for code, ok := range map[string]bool{"691110": true, "69111000": false, "6911": false, "69.11.10": false, "0901210": false} {
		s := ClassificationSuggestion{HSCode: code, LineNo: 1, Rank: 1, Rationale: "r", Confidence: ConfidenceLow}
		if got := s.Validate() == nil; got != ok {
			t.Errorf("HS code %q: valid=%v, want %v", code, got, ok)
		}
	}
}
