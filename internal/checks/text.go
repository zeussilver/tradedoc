package checks

import (
	"strings"
	"unicode"
)

// normaliseRef makes "INV 2026/0042" and "inv-2026-0042" compare equal.
// An invoice number is often retyped on the packing list with different
// separators or case, which is not a real disagreement.
func normaliseRef(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' || r == '/' {
			return -1 // drop the character
		}
		return unicode.ToUpper(r)
	}, s)
}

// unitSynonyms merges spellings of the same unit: abbreviations, plurals
// and British/American spellings of one word. Different units (PCS vs SETS,
// PCS vs KG, PR vs PCS) are never merged, because converting between them
// needs facts we don't have. A unit not listed here is compared as printed.
//
// Each line below is one unit: every spelling we accept, mapped to one of
// them. Every spelling is listed by hand. A blanket rule such as "drop a
// trailing S" gets English wrong (BOXES, GROSS) and would quietly join
// units nobody checked.
//
// Without this table, "KGS" on one document and "KG" on the other would
// raise a unit_mismatch warning and switch off the quantity comparison,
// hiding a real quantity error (which needs a note to approve) behind a
// spelling difference.
var unitSynonyms = map[string]string{
	"KG": "KG", "KGS": "KG", "KILOGRAM": "KG", "KILOGRAMS": "KG",
	"SET": "SET", "SETS": "SET",
	"PR": "PR", "PRS": "PR", "PAIR": "PR", "PAIRS": "PR",
	"DZ": "DZ", "DOZ": "DZ", "DOZEN": "DZ", "DOZENS": "DZ",
	"M": "M", "MTR": "M", "MTRS": "M", "METRE": "M", "METRES": "M", "METER": "M", "METERS": "M",
	"ROLL": "ROLL", "ROLLS": "ROLL",
	"BAG": "BAG", "BAGS": "BAG",
	"BOX": "BOX", "BOXES": "BOX",
	"UNIT": "UNIT", "UNITS": "UNIT",
	"PC": "PCS", "PCS": "PCS", "PIECE": "PCS", "PIECES": "PCS",
	"CTN": "CTN", "CTNS": "CTN", "CARTON": "CTN", "CARTONS": "CTN",
}

// normaliseUnit ignores case, surrounding spaces and a trailing full stop
// ("pcs." = "PCS"), then applies unitSynonyms. Units have no fixed format in
// the schema, so unlike numbers they may arrive padded.
func normaliseUnit(s string) string {
	u := strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(s)), ".")
	if same, ok := unitSynonyms[u]; ok {
		return same
	}
	return u
}

// materialWords say what goods are made of, or that they are a set. When
// two descriptions of the same line differ in one of these, it is more than
// wording: a porcelain mug and a stoneware mug fall under different tariff
// headings (6911 and 6912), and goods put up in sets for retail sale are
// handled by their own rule (General Interpretative Rule 3(b)). So such a
// difference is raised from a note to a warning.
var materialWords = map[string]bool{
	"porcelain": true, "stoneware": true, "earthenware": true, "ceramic": true,
	"cotton": true, "polyester": true, "wool": true, "silk": true, "nylon": true,
	"bamboo": true, "wood": true, "wooden": true,
	"steel": true, "stainless": true, "iron": true, "aluminium": true, "aluminum": true,
	"plastic": true, "leather": true, "glass": true, "rubber": true, "paper": true,
	"set": true,
}

// materialIn returns the material words among the given word lists, in
// order, so a message can name them.
func materialIn(lists ...[]string) []string {
	var out []string
	for _, ws := range lists {
		for _, w := range ws {
			if materialWords[w] {
				out = append(out, w)
			}
		}
	}
	return out
}

// words splits a description into lower-case words made of letters and
// digits, in order, without repeats. Punctuation and spacing differences
// ("27 cm," vs "27 cm") disappear; "100%" becomes "100".
func words(s string) []string {
	notWordChar := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), notWordChar) {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// missingFrom returns the words of a that are not in b, in a's order, so a
// message can quote them the way they read on the document.
func missingFrom(a, b []string) []string {
	inB := map[string]bool{}
	for _, w := range b {
		inB[w] = true
	}
	var out []string
	for _, w := range a {
		if !inB[w] {
			out = append(out, w)
		}
	}
	return out
}
