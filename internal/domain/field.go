// Package domain holds the data model (the "schema") and its validation rules.
// It has no HTTP, storage or AI code in it, so it can be tested on its own.
package domain

// Confidence is the model's own rating of an extracted value.
// It is an enum rather than a number because an LLM's self-reported
// percentage is not calibrated; high/medium/low is all we claim.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

func (c Confidence) Valid() bool {
	switch c {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		return true
	}
	return false
}

// Evidence points back to where a value was read in the source document.
type Evidence struct {
	Page  int    `json:"page"`  // 1-based
	Quote string `json:"quote"` // exact text copied from the document
}

// Field wraps every extracted value with its confidence and evidence.
// Value is a pointer: nil means "not found in the document", which is
// different from an empty string or zero. The model must return null
// rather than guess.
type Field[T any] struct {
	Value      *T         `json:"value"`
	Confidence Confidence `json:"confidence,omitempty"`
	Evidence   *Evidence  `json:"evidence,omitempty"`
}

// Present reports whether the document contained a value.
func (f Field[T]) Present() bool { return f.Value != nil }

// Of is a small helper for building fields in code and tests.
func Of[T any](v T, c Confidence, page int, quote string) Field[T] {
	return Field[T]{Value: &v, Confidence: c, Evidence: &Evidence{Page: page, Quote: quote}}
}
