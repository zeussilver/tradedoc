package ai

import (
	"context"
	"errors"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// Fake returns fixed answers whatever file it is given. It lets tests and
// offline demos run the whole workflow without an API key; results are
// labelled with the model name "fake" so nobody mistakes them for real output.
type Fake struct {
	Invoice     *domain.InvoiceExtraction
	PackingList *domain.PackingListExtraction
	Candidates  []Candidate
}

var errNoFixture = errors.New("fake model has no fixture for this request")

func fakeRun() domain.ModelRun { return domain.ModelRun{Model: "fake", Attempts: 1} }

func (f Fake) ExtractInvoice(context.Context, File) (domain.InvoiceExtraction, domain.ModelRun, error) {
	if f.Invoice == nil {
		return domain.InvoiceExtraction{}, fakeRun(), errNoFixture
	}
	return *f.Invoice, fakeRun(), nil
}

func (f Fake) ExtractPackingList(context.Context, File) (domain.PackingListExtraction, domain.ModelRun, error) {
	if f.PackingList == nil {
		return domain.PackingListExtraction{}, fakeRun(), errNoFixture
	}
	return *f.PackingList, fakeRun(), nil
}

// SuggestCodes returns the fixture candidates for the lines it was asked about.
func (f Fake) SuggestCodes(_ context.Context, lines []Line) ([]Candidate, domain.ModelRun, error) {
	want := map[int]bool{}
	for _, l := range lines {
		want[l.LineNo] = true
	}
	var out []Candidate
	for _, c := range f.Candidates {
		if want[c.LineNo] {
			out = append(out, c)
		}
	}
	return out, fakeRun(), nil
}
