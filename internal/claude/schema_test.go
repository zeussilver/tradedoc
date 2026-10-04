package claude

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// The documented structured-output limits
// (platform.claude.com/docs/en/build-with-claude/structured-outputs,
// "Schema complexity limits"). They count across every strict schema in one
// request; each of our requests sends exactly one schema, so each schema
// must fit on its own. Going over gives a 400 from the API.
const (
	maxUnionParams    = 16 // properties using anyOf or a type list such as ["string", "null"]
	maxOptionalParams = 24 // properties not listed in "required"
)

// The string formats structured outputs support.
var supportedFormats = []string{"date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid"}

// schemas lists each schema with a function that reads an answer the way the
// client does: strictly into the wire type, then into the domain value.
var schemas = []struct {
	name   string
	schema json.RawMessage
	read   func(text string) (any, error)
}{
	{"invoice", invoiceSchemaJSON, readAs(invoiceWire.toDomain)},
	{"packing_list", packingListSchemaJSON, readAs(packingListWire.toDomain)},
	{"candidates", candidatesSchemaJSON, readAs(func(a candidateAnswer) candidateAnswer { return a })},
}

func readAs[W, T any](toDomain func(W) T) func(string) (any, error) {
	return func(text string) (any, error) {
		var w W
		if err := decodeStrict(text, &w); err != nil {
			return nil, err
		}
		return toDomain(w), nil
	}
}

func parseSchema(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return s
}

// walkSchema calls visit for every schema node: the root, each property,
// each array's items and each anyOf branch.
func walkSchema(path string, node map[string]any, visit func(path string, node map[string]any)) {
	visit(path, node)
	if props, ok := node["properties"].(map[string]any); ok {
		for name, p := range props {
			walkSchema(path+"."+name, p.(map[string]any), visit)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		walkSchema(path+"[]", items, visit)
	}
	if branches, ok := node["anyOf"].([]any); ok {
		for _, b := range branches {
			walkSchema(path+"|", b.(map[string]any), visit)
		}
	}
}

// TestSchemasFollowStructuredOutputRules checks every schema node against
// the rules and limits the API enforces, so a mistake shows up here rather
// than as a 400 from the API at demo time. (Before the review, the invoice
// schema had 39 union parameters, and every real extraction would have
// failed.)
func TestSchemasFollowStructuredOutputRules(t *testing.T) {
	unsupported := []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "maxItems", "pattern", "$ref"}

	for _, s := range schemas {
		t.Run(s.name, func(t *testing.T) {
			unions, optional := 0, 0
			walkSchema(s.name, parseSchema(t, s.schema), func(path string, n map[string]any) {
				for _, k := range unsupported {
					if _, ok := n[k]; ok {
						t.Errorf("%s: unsupported keyword %q", path, k)
					}
				}
				if v, ok := n["minItems"]; ok && v != 0.0 && v != 1.0 {
					t.Errorf("%s: minItems may only be 0 or 1, got %v", path, v)
				}
				if f, ok := n["format"].(string); ok && !slices.Contains(supportedFormats, f) {
					t.Errorf("%s: unsupported string format %q", path, f)
				}
				if enum, ok := n["enum"].([]any); ok {
					for _, v := range enum {
						switch v.(type) {
						case string, float64, bool, nil:
						default:
							t.Errorf("%s: enum values must be simple, got %v", path, v)
						}
					}
				}
				if _, ok := n["anyOf"]; ok {
					unions++
				}
				if _, ok := n["type"].([]any); ok {
					unions++
				}

				if n["type"] != "object" {
					return
				}
				if n["additionalProperties"] != false {
					t.Errorf("%s: object must set additionalProperties: false", path)
				}
				props, _ := n["properties"].(map[string]any)
				var names, required []string
				for name := range props {
					names = append(names, name)
				}
				list, _ := n["required"].([]any)
				for _, r := range list {
					required = append(required, r.(string))
				}
				for _, name := range names {
					if !slices.Contains(required, name) {
						optional++
					}
				}
				// Our own rule, stricter than the limit: every property is
				// required, and a value the document doesn't print says so
				// with found: false.
				sort.Strings(names)
				sort.Strings(required)
				if !slices.Equal(names, required) {
					t.Errorf("%s: required %v must list every property %v", path, required, names)
				}
			})

			t.Logf("%s: %d union parameters (limit %d), %d optional (limit %d)", s.name, unions, maxUnionParams, optional, maxOptionalParams)
			if unions > maxUnionParams {
				t.Errorf("%d union-typed parameters; the API allows %d", unions, maxUnionParams)
			}
			if optional > maxOptionalParams {
				t.Errorf("%d optional parameters; the API allows %d", optional, maxOptionalParams)
			}
		})
	}
}

// sample builds an answer that matches the schema, as the model could send
// it. Every "found" flag is set to found; strings are "x" unless the schema
// restricts them.
func sample(t *testing.T, node map[string]any, found bool) any {
	t.Helper()
	if enum, ok := node["enum"].([]any); ok {
		return enum[0]
	}
	switch node["type"] {
	case "object":
		out := map[string]any{}
		for name, p := range node["properties"].(map[string]any) {
			out[name] = sample(t, p.(map[string]any), found)
		}
		return out
	case "array":
		return []any{sample(t, node["items"].(map[string]any), found)}
	case "boolean":
		return found
	case "string":
		if node["format"] == "date" {
			return "2026-09-15"
		}
		return "x"
	case "integer":
		return 1
	}
	t.Fatalf("sample: unexpected schema node %v", node)
	return nil
}

// asDomainJSON is what a wire answer should look like once read into the
// domain types: a field that was found loses its "found" flag, and one that
// was not becomes {"value": null} (its placeholders are dropped).
func asDomainJSON(doc any) any {
	switch d := doc.(type) {
	case map[string]any:
		found, isField := d["found"].(bool)
		if isField && !found {
			return map[string]any{"value": nil}
		}
		out := map[string]any{}
		for k, v := range d {
			if k != "found" {
				out[k] = asDomainJSON(v)
			}
		}
		return out
	case []any:
		out := make([]any, len(d))
		for i, v := range d {
			out[i] = asDomainJSON(v)
		}
		return out
	}
	return doc
}

// normalise round-trips v through JSON so values can be compared as plain
// maps, slices, strings and float64s.
func normalise(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestWireFormatRoundTripsIntoDomain proves each schema, its wire type and
// its domain type describe the same data. An answer built from the schema
// must decode strictly into the wire type (so the schema has no property the
// wire type lacks), and the domain value read from it must hold exactly the
// same data, minus the found flags (so no domain field is left unmapped and
// no wire field is dropped). This is done once with every value found and
// once with none found.
func TestWireFormatRoundTripsIntoDomain(t *testing.T) {
	for _, s := range schemas {
		for _, found := range []bool{true, false} {
			name := s.name + "/found"
			if !found {
				name = s.name + "/not_found"
			}
			t.Run(name, func(t *testing.T) {
				answer := normalise(t, sample(t, parseSchema(t, s.schema), found))
				text, _ := json.Marshal(answer)

				got, err := s.read(string(text))
				if err != nil {
					t.Fatalf("schema sample does not decode into the wire type: %v\n%s", err, text)
				}
				want := asDomainJSON(answer)
				if gotJSON := normalise(t, got); !reflect.DeepEqual(gotJSON, want) {
					g, _ := json.Marshal(gotJSON)
					w, _ := json.Marshal(want)
					t.Errorf("domain value does not match the answer\n got: %s\nwant: %s", g, w)
				}
			})
		}
	}
}

// The schema's enums must offer only values the domain accepts, or the
// model could be forced into an answer that Validate() then rejects.
func TestSchemaEnumsAgreeWithDomain(t *testing.T) {
	for _, code := range incotermValue["enum"].([]string) {
		inv := domain.InvoiceExtraction{
			InvoiceNumber: domain.Of("INV-1", domain.ConfidenceHigh, 1, "INV-1"),
			InvoiceDate:   domain.Of("2026-09-15", domain.ConfidenceHigh, 1, "15 Sep 2026"),
			SellerName:    domain.Of("Seller Co", domain.ConfidenceHigh, 1, "Seller Co"),
			Incoterm:      domain.Of(code, domain.ConfidenceHigh, 1, code),
			LineItems: []domain.InvoiceLine{{
				LineNo:      1,
				Description: domain.Of("Mug", domain.ConfidenceHigh, 1, "Mug"),
				Quantity:    domain.Of("10", domain.ConfidenceHigh, 1, "10 PCS"),
				Unit:        domain.Of("PCS", domain.ConfidenceHigh, 1, "10 PCS"),
			}},
		}
		if err := inv.Validate(); err != nil {
			t.Errorf("incoterm %s is in the schema but the domain rejects it: %v", code, err)
		}
	}
	for _, c := range confidenceValue["enum"].([]string) {
		if !domain.Confidence(c).Valid() {
			t.Errorf("confidence %q is in the schema but the domain rejects it", c)
		}
	}
	for _, k := range chargeKindValue["enum"].([]string) {
		if !domain.ChargeKind(k).Valid() {
			t.Errorf("charge kind %q is in the schema but the domain rejects it", k)
		}
	}
	// Candidate codes rest on the seller's unverified description, so the
	// schema must not let the model rate one high.
	if got := candidateConfidenceValue["enum"].([]string); !slices.Equal(got, []string{"medium", "low"}) {
		t.Errorf("candidate confidence enum = %v, want medium and low only", got)
	}
}

// Property order is part of the prompt (see object): a field says whether
// the value is printed before quoting it, and quotes it before giving it.
func TestFieldPropertyOrder(t *testing.T) {
	b, err := json.Marshal(field("d", stringValue))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f, e, v, c := strings.Index(s, `"found"`), strings.Index(s, `"evidence"`), strings.Index(s, `"value"`), strings.Index(s, `"confidence"`)
	if !(f >= 0 && f < e && e < v && v < c) {
		t.Errorf("want found, evidence, value, confidence in that order, got %s", s)
	}
}
