package domain

import (
	"encoding/json"
	"time"
)

// Shipment groups the documents for one consignment (invoice + packing list).
type Shipment struct {
	ID        string    `json:"id"`
	Reference string    `json:"reference"` // the user's own reference, e.g. a job number
	CreatedAt time.Time `json:"created_at"`
}

type DocType string

const (
	DocCommercialInvoice DocType = "commercial_invoice"
	DocPackingList       DocType = "packing_list"
)

func (t DocType) Valid() bool { return t == DocCommercialInvoice || t == DocPackingList }

type DocStatus string

const (
	StatusUploaded  DocStatus = "uploaded"  // file stored, nothing read yet
	StatusExtracted DocStatus = "extracted" // at least one valid extraction exists
)

// Document is metadata about an uploaded file. The file itself lives in blob
// storage (local disk now, S3 on day 6) under StorageKey, never under the
// user's filename.
type Document struct {
	ID          string    `json:"id"`
	ShipmentID  string    `json:"shipment_id"`
	Type        DocType   `json:"type"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      string    `json:"sha256"`
	StorageKey  string    `json:"-"` // internal, never sent to clients
	Status      DocStatus `json:"status"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

// Extraction is one reading of a document. New readings are appended, never
// overwritten, so we can always see what the model said versus what a person
// corrected. Exactly one of Invoice / PackingList is set, matching the
// document type.
type Extraction struct {
	ID          string                 `json:"id"`
	DocumentID  string                 `json:"document_id"`
	Source      string                 `json:"source"` // "manual" or e.g. "model:claude-sonnet-5"
	CreatedAt   time.Time              `json:"created_at"`
	Invoice     *InvoiceExtraction     `json:"invoice,omitempty"`
	PackingList *PackingListExtraction `json:"packing_list,omitempty"`
}

// Classification suggestions are candidates for a customs broker to review.
// The system never finalises a tariff classification by itself.
type SuggestionStatus string

const (
	SuggestionPending  SuggestionStatus = "pending"
	SuggestionAccepted SuggestionStatus = "accepted"
	SuggestionRejected SuggestionStatus = "rejected"
)

type ClassificationSuggestion struct {
	ID         string           `json:"id"`
	DocumentID string           `json:"document_id"`
	LineNo     int              `json:"line_no"`
	HSCode     string           `json:"hs_code"` // string, not a number: leading zeros matter (e.g. 0901...)
	Rationale  string           `json:"rationale"`
	Confidence Confidence       `json:"confidence"`
	Status     SuggestionStatus `json:"status"`
	ReviewedBy string           `json:"reviewed_by,omitempty"`
	ReviewedAt *time.Time       `json:"reviewed_at,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
}

func (s ClassificationSuggestion) Validate() error {
	var errs ValidationErrors
	if !reHSCode.MatchString(s.HSCode) {
		errs.add("hs_code", "must be 6 or 8 digits")
	}
	if s.LineNo < 1 {
		errs.add("line_no", "must be >= 1")
	}
	if !s.Confidence.Valid() {
		errs.add("confidence", "must be one of high, medium, low")
	}
	return errs.Err()
}

// AuditEvent is an append-only record of who did what. Detail holds IDs and
// short labels only: never document contents, names or prices, so the log
// is safe to ship to CloudWatch.
type AuditEvent struct {
	ID         string            `json:"id"`
	At         time.Time         `json:"at"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"` // e.g. "document.uploaded"
	EntityType string            `json:"entity_type"`
	EntityID   string            `json:"entity_id"`
	ShipmentID string            `json:"shipment_id"`
	Detail     map[string]string `json:"detail,omitempty"`
}

// ExtractionInput is the request body for submitting an extraction.
// Payload is kept raw until we know the document type, then decoded into
// the matching struct.
type ExtractionInput struct {
	Source  string          `json:"source"`
	Payload json.RawMessage `json:"payload"`
}
