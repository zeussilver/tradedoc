// Package ai defines what the rest of the system needs from a language
// model, without saying which model. HTTP handlers depend on these
// interfaces; internal/claude implements them with the Claude API, and Fake
// implements them with fixed answers for tests and offline demos.
package ai

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// File is a document handed to a model: its bytes and the content type we
// sniffed from those bytes on upload (never the client's claim).
type File struct {
	ContentType string // application/pdf, image/png or image/jpeg
	Data        []byte
}

// Extractor reads a trade document into the domain schema. Implementations
// must return output that passes the schema's Validate(), or an error.
type Extractor interface {
	ExtractInvoice(ctx context.Context, f File) (domain.InvoiceExtraction, domain.ModelRun, error)
	ExtractPackingList(ctx context.Context, f File) (domain.PackingListExtraction, domain.ModelRun, error)
}

// Line is what a classifier is told about one invoice line: only what a
// tariff heading turns on. Prices, quantities, parties and origin are left
// out on purpose: they don't change the HS subheading (origin affects the
// duty rate and preference, not the heading), so they don't leave our system.
type Line struct {
	LineNo      int
	Description string
	Unit        string
}

// Candidate is one suggested HS code for one line.
type Candidate struct {
	LineNo      int               `json:"line_no"`
	Rank        int               `json:"rank"`    // 1 = first choice for this line
	HSCode      string            `json:"hs_code"` // 6-digit HS subheading
	Rationale   string            `json:"rationale"`
	Confidence  domain.Confidence `json:"confidence"`
	MissingInfo []string          `json:"missing_info"`
}

// Classifier suggests candidate HS codes for a person to review.
type Classifier interface {
	SuggestCodes(ctx context.Context, lines []Line) ([]Candidate, domain.ModelRun, error)
}

var (
	// ErrRefused means the model declined to answer (stop_reason "refusal").
	ErrRefused = errors.New("model declined the request")
	// ErrUnavailable means the model API could not be reached or kept
	// failing (network errors, 429, 5xx) after the client's retries.
	ErrUnavailable = errors.New("model service unavailable")
	// ErrTooLarge means the document is too big to read in one pass: its
	// input is over the configured token budget, or the answer would not
	// fit in the output limit. Retrying would only cost money.
	ErrTooLarge = errors.New("document too large for one model pass")
)

// InvalidOutputError means the model's answer still broke the schema after
// it was shown its errors and asked to fix them.
type InvalidOutputError struct {
	Errs domain.ValidationErrors
	Run  domain.ModelRun
}

func (e *InvalidOutputError) Error() string {
	return fmt.Sprintf("model output failed validation after %d attempt(s): %v", e.Run.Attempts, e.Errs)
}
