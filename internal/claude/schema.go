package claude

import (
	"bytes"
	"encoding/json"
)

// JSON schemas for structured outputs. The API constrains the model's answer
// to these shapes, so the answer always parses into the wire types in
// wire.go; the domain Validate() methods then check the rules a schema can't
// express (formats, ranges, "a value needs evidence").
//
// Structured outputs accept a subset of JSON Schema, with limits:
//   - every object lists all of its properties as required and sets
//     additionalProperties to false;
//   - keywords such as minimum or maxLength are not supported, so limits like
//     "quote at most 300 characters" are stated in descriptions and enforced
//     by Validate();
//   - at most 16 values per request may be a union (anyOf, or a type list
//     such as ["string", "null"]), because each one makes the schema much
//     more expensive to compile. An invoice has more than 16 fields, so a
//     field is never nullable: it says found: false instead (see wire.go).
//
// schema_test.go checks these limits, and that the schemas, the wire types
// and the domain types stay in step.

// prop is one property of a JSON-schema object.
type prop struct {
	name   string
	schema any
}

// object is a JSON-schema object. We write our own type rather than a Go map
// because maps marshal their keys in alphabetical order, and the model writes
// its answer in the order the schema lists the properties. Order is part of
// the prompt: a field says first whether the value is printed at all, then
// the evidence quote, then the value read from that quote, then the
// confidence in that reading.
type object struct {
	description string
	props       []prop
}

func (o object) MarshalJSON() ([]byte, error) {
	// Build "properties" by hand to keep the order of o.props.
	var props bytes.Buffer
	required := make([]string, 0, len(o.props))
	props.WriteByte('{')
	for i, p := range o.props {
		if i > 0 {
			props.WriteByte(',')
		}
		name, err := json.Marshal(p.name)
		if err != nil {
			return nil, err
		}
		schema, err := json.Marshal(p.schema)
		if err != nil {
			return nil, err
		}
		props.Write(name)
		props.WriteByte(':')
		props.Write(schema)
		required = append(required, p.name)
	}
	props.WriteByte('}')

	// The outer keys are fixed, and a struct marshals in field order.
	return json.Marshal(struct {
		Type                 string          `json:"type"`
		Description          string          `json:"description,omitempty"`
		Properties           json.RawMessage `json:"properties"`
		Required             []string        `json:"required"`
		AdditionalProperties bool            `json:"additionalProperties"`
	}{"object", o.description, props.Bytes(), required, false})
}

var (
	stringValue  = map[string]any{"type": "string"}
	integerValue = map[string]any{"type": "integer"}
	dateValue    = map[string]any{"type": "string", "format": "date"} // YYYY-MM-DD

	// The eleven Incoterms 2020 rules, the same list domain.Validate accepts.
	incotermValue = map[string]any{"type": "string", "enum": []string{
		"EXW", "FCA", "CPT", "CIP", "DAP", "DPU", "DDP", "FAS", "FOB", "CFR", "CIF",
	}}

	confidenceValue = map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}}

	// A candidate code is never rated high: the description it rests on is
	// the seller's own, unverified declaration (see the classification prompt).
	candidateConfidenceValue = map[string]any{"type": "string", "enum": []string{"medium", "low"}}

	// The same kinds domain.ChargeKind accepts.
	chargeKindValue = map[string]any{
		"type":        "string",
		"enum":        []string{"freight", "insurance", "discount", "other"},
		"description": "freight, insurance, discount, or other for any other non-goods charge (packing, handling, bank fees...).",
	}
)

func arrayOf(description string, items any) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items}
}

func describedInteger(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func describedString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func describedBoolean(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

var evidenceSchema = object{
	description: "Where the value was read on the document.",
	props: []prop{
		{"page", describedInteger("1-based page number of the file where the quote appears.")},
		{"quote", describedString("The exact text copied from the document, at most 300 characters.")},
	},
}

var foundSchema = describedBoolean("true if the value is printed on the document. If false, the other properties of this field are placeholders and are ignored.")

// field is the schema of one extracted value: whether it is printed at all,
// the evidence it was read from, the value, and the model's confidence in
// that reading. See wire.go for why "found" replaces a null value.
func field(description string, value any) object {
	return object{
		description: description,
		props: []prop{
			{"found", foundSchema},
			{"evidence", evidenceSchema},
			{"value", value},
			{"confidence", confidenceValue},
		},
	}
}

// Lines are paired across the invoice and the packing list by line_no, so
// both documents must number their rows the same way. Printed numbers can't
// be trusted for that (an ERP numbers positions 10, 20, 30; a packing list
// prints carton ranges), but the order of the goods rows can.
var lineNoSchema = describedInteger("The row's 1-based position among the goods rows, in printed order: the first goods row is 1. Not a printed item, position or carton number.")

var invoiceLineSchema = object{props: []prop{
	{"line_no", lineNoSchema},
	{"description", field("Goods description on this row, in full, as printed.", stringValue)},
	{"quantity", field(`Quantity as the printed decimal, without unit or thousands separators ("1,200 PCS" -> "1200", "2,450.75 KG" -> "2450.75").`, stringValue)},
	{"unit", field("Unit of quantity as printed, upper case (PCS, SETS, KG).", stringValue)},
	{"unit_price", field(`Unit price as the printed decimal, without currency symbol or thousands separators ("3.50", "0.385").`, stringValue)},
	{"line_total_minor", field("Line amount exactly as printed, as an integer in minor units (USD 4,200.00 -> 420000).", integerValue)},
	{"origin_country", field("ISO 3166-1 alpha-2 code of the country of origin printed for this line or for all lines (Made in China -> CN).", stringValue)},
}}

var chargeSchema = object{props: []prop{
	{"kind", chargeKindValue},
	{"label", field("The charge's label as printed, e.g. Ocean freight.", stringValue)},
	{"amount_minor", field("The amount as printed, as a positive integer in minor units (USD 450.00 -> 45000). A discount is positive too: its kind says it is subtracted.", integerValue)},
}}

var invoiceSchema = object{
	description: "A commercial invoice, transcribed exactly as printed.",
	props: []prop{
		{"invoice_number", field("Invoice number as printed, without its label.", stringValue)},
		{"invoice_date", field("Invoice date as YYYY-MM-DD.", dateValue)},
		{"seller_name", field("Seller (exporter) company name as printed, without the address.", stringValue)},
		{"buyer_name", field("Buyer (importer) company name as printed, without the address.", stringValue)},
		{"currency", field("ISO 4217 code of the invoice currency, e.g. USD. A symbol alone such as $ is not a currency: found is false unless the document says which currency it means.", stringValue)},
		{"incoterm", field("Incoterms 2020 rule code only, without the named place (FOB Ningbo -> FOB).", incotermValue)},
		{"line_items", arrayOf("One entry per printed row of goods, in printed order.", invoiceLineSchema)},
		{"charges", arrayOf("Rows between the goods and the total that are not goods: freight, insurance, discount and other charges. Not subtotal or total rows. Empty if there are none.", chargeSchema)},
		{"total_minor", field("Invoice total exactly as printed, as an integer in minor units (USD 7,302.00 -> 730200). Never the sum of the lines.", integerValue)},
	},
}

var packingLineSchema = object{props: []prop{
	{"line_no", lineNoSchema},
	{"description", field("Goods description on this row, in full, as printed.", stringValue)},
	{"quantity", field(`Quantity as the printed decimal, without unit or thousands separators ("1,200 PCS" -> "1200", "2,450.75 KG" -> "2450.75").`, stringValue)},
	{"unit", field("Unit of quantity as printed, upper case (PCS, SETS, KG).", stringValue)},
	{"packages", field("Number of packages for this row as printed (50 CTNS -> 50).", integerValue)},
	{"gross_weight_grams", field("Gross weight for this row in whole grams (540.00 kg -> 540000).", integerValue)},
}}

var packingListSchema = object{
	description: "A packing list, transcribed exactly as printed.",
	props: []prop{
		{"packing_list_number", field("Packing list number as printed, without its label.", stringValue)},
		{"invoice_ref", field("The invoice number this packing list refers to, as printed.", stringValue)},
		{"line_items", arrayOf("One entry per printed row of goods, in printed order.", packingLineSchema)},
		{"total_packages", field("Total number of packages as printed, counting the same kind of package as the rows (84 CTNS -> 84). If the total counts another kind, such as pallets when the rows count cartons, and no total of the rows' kind is printed, found is false. Never the sum of the lines.", integerValue)},
		{"total_gross_weight_grams", field("Total gross weight as printed, in whole grams (936.00 kg -> 936000). Never the sum of the lines.", integerValue)},
	},
}

// candidatesSchema wraps the list in an object because a structured-output
// answer must be a JSON object at the top level.
var candidatesSchema = object{props: []prop{
	{"candidates", arrayOf("Up to three candidate codes per invoice line, for a licensed customs broker to review.", object{props: []prop{
		{"line_no", describedInteger("The line_no of the invoice line this candidate is for, exactly as given.")},
		{"rank", describedInteger("1 for the most likely candidate for this line, then 2 and 3.")},
		{"hs_code", describedString("HS 2022 subheading: exactly six digits, no dots or spaces, leading zeros kept (090121).")},
		{"rationale", describedString("Why the broker should consider this code: the heading and subheading text that fits, the General Interpretative Rules relied on, and what would change it. At most 600 characters.")},
		{"confidence", candidateConfidenceValue},
		{"missing_info", arrayOf("Facts a broker would need to confirm before relying on this code (material, composition, use, dimensions...), as short phrases.", stringValue)},
	}})},
}}

// The schemas are marshalled once at start-up. A mistake in them is a
// programming error, so panicking is right (and the tests would catch it).
var (
	invoiceSchemaJSON     = mustMarshal(invoiceSchema)
	packingListSchemaJSON = mustMarshal(packingListSchema)
	candidatesSchemaJSON  = mustMarshal(candidatesSchema)
)

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
