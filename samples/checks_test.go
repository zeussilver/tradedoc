package samples

import (
	"slices"
	"strconv"
	"testing"

	"github.com/zeussilver/tradedoc/internal/checks"
	"github.com/zeussilver/tradedoc/internal/domain"
)

// TestSampleFindings runs the real cross-document checks on the hand-checked
// expected extractions of each sample pair. Sample A was built with
// deliberate problems; sample B was built to be consistent, apart from the
// CIF invoice not showing freight and insurance separately. If a change to
// the checks or the fixtures shifts these results, this test says so.
//
// Severities are part of the expectation: a reviewer must explain every
// error before approving, so an error quietly becoming a warning (or the
// other way round) changes what the sample demonstrates.
func TestSampleFindings(t *testing.T) {
	tests := []struct {
		sample string
		want   []string // "code severity" or "code@line severity"
	}{
		{"sample-a", []string{
			"invoice_total_mismatch error",   // printed 7,302.00, lines add up to 7,320.00
			"package_total_mismatch warning", // printed 84 CTNS, lines add up to 82
			"quantity_mismatch@2 error",      // 800 on the invoice, 780 on the packing list
			"description_differs@1 info",     // the invoice also says "white glazed"
			"description_differs@2 warning",  // stoneware vs ceramic: material words differ
		}},
		{"sample-b", []string{
			// CIF Melbourne, but no freight or insurance row is printed.
			"freight_insurance_not_shown info",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.sample, func(t *testing.T) {
			var inv domain.InvoiceExtraction
			var pl domain.PackingListExtraction
			decodeStrict(t, tc.sample+"-invoice.json", &inv)
			decodeStrict(t, tc.sample+"-packing-list.json", &pl)

			findings := checks.Compare(
				domain.Extraction{DocumentID: "doc_inv", Invoice: &inv},
				domain.Extraction{DocumentID: "doc_pl", PackingList: &pl},
			)
			var got []string
			for _, f := range findings {
				// These two are notes about the reading, not about the
				// documents, so they may change with fixture confidences
				// without changing what the sample shows. They must stay
				// notes, though.
				if f.Code == "low_confidence" || f.Code == "not_checked" {
					if f.Severity != domain.SeverityInfo {
						t.Errorf("%s has severity %s, want info", f.Code, f.Severity)
					}
					continue
				}
				key := f.Code
				if f.LineNo > 0 {
					key += "@" + strconv.Itoa(f.LineNo)
				}
				got = append(got, key+" "+string(f.Severity))
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("findings =\n  %q\nwant\n  %q", got, want)
			}
		})
	}
}
