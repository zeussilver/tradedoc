package checks

import (
	"math/big"
	"testing"

	"github.com/zeussilver/tradedoc/internal/domain"
)

func TestLineAmount(t *testing.T) {
	tests := []struct {
		qty, price string
		decimals   int
		want       int64 // minor units
	}{
		{"5000", "0.385", 2, 192500}, // 1,925.00 exactly
		{"1200", "3.50", 2, 420000},
		{"7", "10", 2, 7000},                             // no decimal point
		{"3", "0.335", 2, 101},                           // 1.005 rounds half-up to 1.01
		{"1", "0.005", 2, 1},                             // half a cent goes up
		{"1", "0.004999", 2, 0},                          // just under half a cent goes down
		{"3", "33.5", 0, 101},                            // JPY: 100.5 yen rounds up to 101
		{"1", "1.2345", 3, 1235},                         // BHD: 3 decimal places, 1.2345 -> 1.235
		{"2", "0.000001", 2, 0},                          // 6 decimal places, tiny
		{"1", "999999999999.999999", 2, 100000000000000}, // largest price the schema allows

		// Decimal quantities: goods sold by weight or length.
		{"1523.60", "2.15", 2, 327574},  // 1,523.60 M x 2.15 = 3,275.74 exactly
		{"2450.75", "3.20", 2, 784240},  // 2,450.75 KG x 3.20 = 7,842.40 exactly
		{"1250.5", "0.385", 2, 48144},   // 481.4425 rounds down to 481.44
		{"1.5", "0.333", 2, 50},         // 0.4995 rounds half-up to 0.50
		{"0.5", "0.01", 2, 1},           // half a cent from the quantity's decimals goes up
		{"0.001", "0.001", 2, 0},        // 0.000001: far below half a cent
		{"12.5", "800", 0, 10000},       // JPY: 12.5 KG x 800 yen
		{"1200.000", "3.50", 2, 420000}, // trailing zeros change nothing
	}
	for _, tc := range tests {
		got, ok := lineAmount(tc.qty, tc.price, tc.decimals)
		if !ok || got.Cmp(big.NewInt(tc.want)) != 0 {
			t.Errorf("lineAmount(%q, %q, %d) = %v, %v; want %d", tc.qty, tc.price, tc.decimals, got, ok, tc.want)
		}
	}

	// A product too big for int64 is still exact:
	// 9e15 x (1e12 - 1) x 100 cents = 8.999999999991e29.
	got, ok := lineAmount("9000000000000000", "999999999999", 2)
	want, _ := new(big.Int).SetString("899999999999100000000000000000", 10)
	if !ok || got.Cmp(want) != 0 {
		t.Errorf("huge product = %v; want %v", got, want)
	}

	// Anything but a plain decimal is refused, as a quantity or as a price.
	// There is no TrimSpace: " 1" is refused, not read as 1.
	for _, bad := range []string{"", "1.", ".5", "-1", "+1", "1,000.00", "1e3", "abc", " 1", "1 "} {
		if _, ok := lineAmount("1", bad, 2); ok {
			t.Errorf("lineAmount accepted price %q", bad)
		}
		if _, ok := lineAmount(bad, "1", 2); ok {
			t.Errorf("lineAmount accepted quantity %q", bad)
		}
	}
}

func TestSameQuantity(t *testing.T) {
	tests := []struct {
		a, b string
		same bool
	}{
		{"1200", "1200", true},
		{"1200", "1200.0", true},
		{"1200.50", "1200.5", true},
		{"0.5", "00.500", true},
		{"1200", "1201", false},
		{"500", "500.001", false},
		{"1250.5", "1250.4", false},
		{"0.1", "1", false},
	}
	for _, tc := range tests {
		same, ok := sameQuantity(tc.a, tc.b)
		if !ok || same != tc.same {
			t.Errorf("sameQuantity(%q, %q) = %v, %v; want %v", tc.a, tc.b, same, ok, tc.same)
		}
	}
	for _, bad := range []string{"", "1,200", " 1200", "1e3", "-5"} {
		if _, ok := sameQuantity("1200", bad); ok {
			t.Errorf("sameQuantity accepted %q", bad)
		}
	}
}

func TestFormatting(t *testing.T) {
	usd := currencyOf(hi("USD", "USD"))
	jpy := currencyOf(hi("JPY", "JPY"))
	bhd := currencyOf(hi("BHD", "BHD"))
	none := currencyOf(domain.Field[string]{})

	tests := []struct{ got, want string }{
		{usd.amount(big.NewInt(730200)), "USD 7,302.00"},
		{usd.amount(big.NewInt(5)), "USD 0.05"},
		{usd.amount(big.NewInt(0)), "USD 0.00"},
		{usd.amount(big.NewInt(-18000)), "USD -180.00"},
		{usd.amount(big.NewInt(123456789012)), "USD 1,234,567,890.12"},
		{jpy.amount(big.NewInt(1500)), "JPY 1,500"},
		{bhd.amount(big.NewInt(1235)), "BHD 1.235"},
		{none.amount(big.NewInt(730200)), "7,302.00"},
		{usd.price("0.385"), "USD 0.385"},
		{usd.price("1250.5"), "USD 1,250.5"},
		{usd.price(" 2.85"), "USD  2.85"}, // not a plain decimal: shown as stored
		{none.price("3.50"), "3.50"},
		{formatWeight(big.NewInt(540000)), "540 kg"},
		{formatWeight(big.NewInt(540500)), "540.5 kg"},
		{formatWeight(big.NewInt(1190000)), "1,190 kg"},
		{formatWeight(big.NewInt(5)), "0.005 kg"},
		{formatCount("1200", "PCS"), "1,200 PCS"},
		{formatCount("780", "PCS"), "780 PCS"},
		{formatCount("1250.5", "KG"), "1,250.5 KG"},
		{formatCount("1200.00", "PCS"), "1,200.00 PCS"}, // decimals kept as printed
		{formatCount("999", ""), "999"},
		{formatCount("2,000", "PCS"), "2,000 PCS"}, // not a plain decimal: shown as stored
		{formatPackages(big.NewInt(84)), "84 packages"},
		{formatScaled(big.NewInt(-1800), 2, false), "-18.00"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestMinorUnitExceptionsIsACopy(t *testing.T) {
	m := MinorUnitExceptions()
	if m["JPY"] != 0 || m["BHD"] != 3 {
		t.Fatalf("unexpected table: %v", m)
	}
	if _, ok := m["USD"]; ok {
		t.Error("USD has the default 2 decimals and should not be listed")
	}
	m["JPY"] = 2
	if currencyOf(hi("JPY", "JPY")).decimals != 0 {
		t.Error("changing the returned map changed the checks' table")
	}
}

func TestNormalise(t *testing.T) {
	refs := map[[2]string]bool{
		{"INV-2026-0042", "inv 2026/0042"}: true,
		{"INV-2026-0042", "INV20260042"}:   true,
		{"INV-2026-0042", "INV-2026-0043"}: false,
		{"INV-2026-0042", "INV.2026.0042"}: false, // only spaces, - and / are ignored
	}
	for pair, same := range refs {
		if got := normaliseRef(pair[0]) == normaliseRef(pair[1]); got != same {
			t.Errorf("refs %q and %q: same=%v, want %v", pair[0], pair[1], got, same)
		}
	}

	units := map[[2]string]bool{
		{"PCS", "pcs"}:      true,
		{"PCS", "PC"}:       true,
		{"PCS", " Pieces "}: true,
		{"PCS", "pcs."}:     true,
		{"CTNS", "cartons"}: true,
		{"KG", "kgs"}:       true,
		{"M", "Metres"}:     true,
		{"PCS", "SETS"}:     false,
		{"PCS", "KG"}:       false,
		{"PCS", "CTNS"}:     false,
		{"GROSS", "GROS"}:   false, // no blanket "drop the trailing S" rule
	}
	for pair, same := range units {
		if got := normaliseUnit(pair[0]) == normaliseUnit(pair[1]); got != same {
			t.Errorf("units %q and %q: same=%v, want %v", pair[0], pair[1], got, same)
		}
	}

	got := words("100% Cotton terry bath towel, 70 x 140 cm, cotton")
	want := []string{"100", "cotton", "terry", "bath", "towel", "70", "x", "140", "cm"}
	if len(got) != len(want) {
		t.Fatalf("words = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("words = %q, want %q", got, want)
		}
	}

	if m := materialIn([]string{"white", "stoneware"}, []string{"ceramic", "set", "glazed"}); len(m) != 3 ||
		m[0] != "stoneware" || m[1] != "ceramic" || m[2] != "set" {
		t.Errorf("materialIn = %q, want [stoneware ceramic set]", m)
	}
}
