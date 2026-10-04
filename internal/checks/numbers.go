package checks

import (
	"math/big"
	"regexp"
	"strings"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// Arithmetic and display helpers. Money and weights are integers (minor
// units, grams); quantities and unit prices are the decimal strings printed
// on the document ("1250.5", "0.385"). All maths is done on whole numbers
// with math/big, so nothing is lost to float64 rounding and nothing can
// overflow.

// minorUnits lists ISO 4217 currencies whose minor unit is not 2 decimal
// places. Everything else (USD, AUD, CNY, EUR...) has cents. This matters
// because amounts arrive as integers in minor units: 1500 is JPY 1,500 but
// USD 15.00.
var minorUnits = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "PYG": 0, "UGX": 0, "XAF": 0,
	"XOF": 0, "XPF": 0, "KMF": 0, "GNF": 0, "DJF": 0, "BIF": 0, "RWF": 0, "VUV": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
}

// MinorUnitExceptions returns a copy of the currencies whose minor unit is
// not 2 decimals. GET /v1/config sends it to the web UI, so the browser and
// the checks format money from one table instead of two that could drift.
func MinorUnitExceptions() map[string]int {
	out := make(map[string]int, len(minorUnits))
	for k, v := range minorUnits {
		out[k] = v
	}
	return out
}

// currency knows how to show money in the invoice's currency.
type currency struct {
	code     string // "" when the invoice shows no currency
	decimals int    // digits after the decimal point
}

// currencyOf reads the invoice currency. Without one we assume 2 decimals
// (the most common case) and show bare numbers rather than guess a code.
func currencyOf(f domain.Field[string]) currency {
	if !f.Present() {
		return currency{decimals: 2}
	}
	c := strings.ToUpper(strings.TrimSpace(*f.Value))
	d, ok := minorUnits[c]
	if !ok {
		d = 2
	}
	return currency{code: c, decimals: d}
}

// amount shows minor units as money, e.g. 730200 -> "USD 7,302.00".
func (c currency) amount(minor *big.Int) string {
	return c.withCode(formatScaled(minor, c.decimals, false))
}

// price shows a unit price as printed, with thousands separators added:
// "1250.385" -> "USD 1,250.385".
func (c currency) price(p string) string {
	return c.withCode(groupDecimal(p))
}

func (c currency) withCode(s string) string {
	if c.code == "" {
		return s
	}
	return c.code + " " + s
}

// reDecimal is the shape of a quantity or unit price: digits with an
// optional decimal part, and no sign, separators or spaces. The domain
// schema allows the same shape (with length limits) and Validate() matches
// it against the stored value, so a padded " 3.50" is rejected there and
// never reaches the checks. The checks still test the shape themselves,
// because they must never guess what a malformed number was meant to be.
var reDecimal = regexp.MustCompile(`^\d+(\.\d+)?$`)

// decimal is a printed decimal number held exactly as a whole number of
// digits and the count of digits after the point: "0.385" is 385 and 3,
// "1250.5" is 12505 and 1.
type decimal struct {
	digits *big.Int
	places int
}

// parseDecimal reads a plain decimal string. ok is false for anything else,
// e.g. "1,200", "1e3", "-1" or " 1". There is deliberately no TrimSpace:
// the checks read exactly what was stored.
func parseDecimal(s string) (d decimal, ok bool) {
	if !reDecimal.MatchString(s) {
		return decimal{}, false
	}
	whole, frac, _ := strings.Cut(s, ".")
	digits, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return decimal{}, false
	}
	return decimal{digits: digits, places: len(frac)}, true
}

// lineAmount works out quantity x unit price in minor units, exactly.
//
// Both are decimal strings. "1250.5" is read as 12505 with 1 decimal place
// and "0.385" as 385 with 3, so for USD (2 decimal places) the line amount
// in cents is
//
//	12505 x 385 x 10^2 / 10^(1+3)
//
// which is all integer maths. The last step, dividing by 10^4, can leave a
// fraction of a cent; it is rounded half-up (half a cent or more goes up),
// the rule invoices normally use. Integer division always rounds down, so
// adding half the divisor first turns it into round-half-up:
// 3 x 0.335 = 1.005 -> (100500 + 500) / 1000 = 101 cents = 1.01.
// That only works for non-negative numbers, which these are: reDecimal
// allows no sign.
//
// ok is false if either string is not a plain decimal number.
func lineAmount(qty, price string, decimals int) (minor *big.Int, ok bool) {
	q, okQty := parseDecimal(qty)
	p, okPrice := parseDecimal(price)
	if !okQty || !okPrice {
		return nil, false
	}
	num := new(big.Int).Mul(q.digits, p.digits)
	num.Mul(num, pow10(decimals))
	den := pow10(q.places + p.places)
	half := new(big.Int).Quo(den, big.NewInt(2)) // 0 when den is 1: nothing to round
	num.Add(num, half)
	return num.Quo(num, den), true
}

// sameQuantity reports whether two printed quantities are the same number,
// so "1200" equals "1200.0" but not "1200.001". Instead of dividing, it
// cross-multiplies: a/10^p equals b/10^q exactly when a x 10^q equals
// b x 10^p.
//
// ok is false if either string is not a plain decimal number.
func sameQuantity(a, b string) (same, ok bool) {
	x, okA := parseDecimal(a)
	y, okB := parseDecimal(b)
	if !okA || !okB {
		return false, false
	}
	left := new(big.Int).Mul(x.digits, pow10(y.places))
	right := new(big.Int).Mul(y.digits, pow10(x.places))
	return left.Cmp(right) == 0, true
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// formatScaled shows v / 10^decimals with thousands separators:
// (730200, 2) -> "7,302.00". With trimZeros, trailing zeros after the point
// are dropped, which suits weights: (540500, 3) -> "540.5", (540000, 3) -> "540".
func formatScaled(v *big.Int, decimals int, trimZeros bool) string {
	s := new(big.Int).Abs(v).String()
	// Pad so there is at least one digit before the point: 5 cents -> "005" -> "0.05".
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-decimals], s[len(s)-decimals:]
	if trimZeros {
		frac = strings.TrimRight(frac, "0")
	}
	out := groupThousands(whole)
	if frac != "" {
		out += "." + frac
	}
	if v.Sign() < 0 {
		out = "-" + out
	}
	return out
}

// groupThousands adds commas to a string of digits: "7302" -> "7,302".
func groupThousands(digits string) string {
	var b strings.Builder
	for i := 0; i < len(digits); i++ {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return b.String()
}

// groupDecimal adds thousands separators to a printed decimal, keeping its
// decimal places as printed: "1250.385" -> "1,250.385". A value that is not
// a plain decimal is shown exactly as stored, so a finding about a bad value
// shows the bad value.
func groupDecimal(s string) string {
	if !reDecimal.MatchString(s) {
		return s
	}
	whole, frac, hasPoint := strings.Cut(s, ".")
	out := groupThousands(whole)
	if hasPoint {
		out += "." + frac
	}
	return out
}

// formatCount shows a printed quantity with its unit as printed:
// "1200" -> "1,200 PCS", "1250.5" -> "1,250.5 KG".
func formatCount(qty, unit string) string {
	s := groupDecimal(qty)
	if unit == "" {
		return s
	}
	return s + " " + unit
}

// formatPackages shows a package count. The kind of package (CTNS, PLTS...)
// is not extracted, only the number, so the count is called "packages"; the
// evidence quote on the Ref shows the kind as printed.
func formatPackages(n *big.Int) string { return formatScaled(n, 0, false) + " packages" }

// formatWeight shows grams as kilograms: 540000 -> "540 kg", 540500 -> "540.5 kg".
func formatWeight(grams *big.Int) string { return formatScaled(grams, 3, true) + " kg" }
