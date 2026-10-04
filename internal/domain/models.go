package domain

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
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
	StatusPendingUpload DocStatus = "pending_upload" // upload URL issued, file not yet confirmed
	StatusUploaded      DocStatus = "uploaded"       // file stored and checked, nothing read yet
	StatusExtracted     DocStatus = "extracted"      // at least one valid extraction exists
	StatusRejected      DocStatus = "rejected"       // uploaded file failed the size/type checks and was deleted
)

// Document is metadata about an uploaded file. The file itself lives in blob
// storage (local disk or S3) under StorageKey, never under the user's filename.
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
	Source      string                 `json:"source"`        // "manual" or e.g. "model:claude-opus-5-5"
	Run         *ModelRun              `json:"run,omitempty"` // set when a model produced it
	CreatedAt   time.Time              `json:"created_at"`
	Invoice     *InvoiceExtraction     `json:"invoice,omitempty"`
	PackingList *PackingListExtraction `json:"packing_list,omitempty"`
}

// ModelRun records how a model result was produced, for the audit trail.
// It holds counts and names only, never prompt or document text.
type ModelRun struct {
	Model        string `json:"model"`    // model that actually answered
	Attempts     int    `json:"attempts"` // 1 + number of retries after schema errors
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// Classification suggestions are candidates for a customs broker to review.
// The system never finalises a tariff classification by itself.
type SuggestionStatus string

const (
	SuggestionPending  SuggestionStatus = "pending"
	SuggestionAccepted SuggestionStatus = "accepted"
	SuggestionRejected SuggestionStatus = "rejected"
)

// One suggestion is one candidate code for one invoice line; a line usually
// gets two or three candidates, ranked. Only 6-digit HS subheadings are
// stored. The national classification (in Australia, the 8-digit tariff
// classification in Schedule 3 of the Customs Tariff Act 1995 plus its
// separate 2-digit statistical code) is the broker's decision.
type ClassificationSuggestion struct {
	ID           string           `json:"id"`
	ShipmentID   string           `json:"shipment_id"`
	DocumentID   string           `json:"document_id"`   // the invoice
	ExtractionID string           `json:"extraction_id"` // which reading of the invoice was used
	LineNo       int              `json:"line_no"`
	Description  string           `json:"description"`        // the line's description, copied for display
	Evidence     *Evidence        `json:"evidence,omitempty"` // where that description was read
	Rank         int              `json:"rank"`               // 1 = the model's first candidate for this line
	HSCode       string           `json:"hs_code"`            // string, not a number: leading zeros matter (e.g. 0901...)
	Rationale    string           `json:"rationale"`
	Confidence   Confidence       `json:"confidence"`
	MissingInfo  []string         `json:"missing_info"` // facts a broker would need to confirm the code
	Source       string           `json:"source"`       // e.g. "model:claude-opus-5-5"
	Status       SuggestionStatus `json:"status"`
	ReviewedBy   string           `json:"reviewed_by,omitempty"`
	ReviewNote   string           `json:"review_note,omitempty"`
	ReviewedAt   *time.Time       `json:"reviewed_at,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
}

const maxRationaleLen = 600

func (s ClassificationSuggestion) Validate() error {
	var errs ValidationErrors
	if !reHSCode.MatchString(s.HSCode) {
		errs.add("hs_code", "must be a 6-digit HS subheading")
	}
	if s.LineNo < 1 {
		errs.add("line_no", "must be >= 1")
	}
	if s.Rank < 1 {
		errs.add("rank", "must be >= 1")
	}
	if !s.Confidence.Valid() {
		errs.add("confidence", "must be one of high, medium, low")
	}
	if r := strings.TrimSpace(s.Rationale); r == "" || utf8.RuneCountInString(r) > maxRationaleLen {
		errs.add("rationale", "must be 1-%d characters", maxRationaleLen)
	}
	return errs.Err()
}

// Review is a person's decision on one suggestion. Decisions are final: a
// reviewed suggestion cannot be reviewed again, so the audit trail can't be
// rewritten after the fact.
type Review struct {
	Status   SuggestionStatus `json:"status"` // accepted or rejected
	Reviewer string           `json:"reviewer"`
	Note     string           `json:"note,omitempty"`
	At       time.Time        `json:"at"`
}

// ---- cross-document checks ----

type Severity string

const (
	SeverityError   Severity = "error"   // the documents disagree; must be resolved or explained
	SeverityWarning Severity = "warning" // likely a problem, a person should look
	SeverityInfo    Severity = "info"    // worth knowing, e.g. a check was skipped
)

// Ref points at one value in one extraction, so a reviewer can see both
// sides of a mismatch and where each was read.
type Ref struct {
	DocumentID string    `json:"document_id"`
	Path       string    `json:"path"`  // e.g. "line_items[1].quantity"
	Value      string    `json:"value"` // display form, e.g. "800 PCS" or "USD 5,880.00"
	Evidence   *Evidence `json:"evidence,omitempty"`
}

// Finding is one result of comparing an invoice with its packing list.
// Findings are reported to a person; they never block or change the data.
type Finding struct {
	Code     string   `json:"code"` // stable machine-readable code, e.g. "quantity_mismatch"
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	LineNo   int      `json:"line_no,omitempty"`
	Refs     []Ref    `json:"refs"`
}

// CheckRun is one comparison of specific extractions. Runs are append-only:
// re-running after a correction adds a new run, it doesn't edit the old one.
type CheckRun struct {
	ID                      string    `json:"id"`
	ShipmentID              string    `json:"shipment_id"`
	InvoiceExtractionID     string    `json:"invoice_extraction_id"`
	PackingListExtractionID string    `json:"packing_list_extraction_id"`
	Findings                []Finding `json:"findings"`
	CreatedAt               time.Time `json:"created_at"`
}

// ---- sign-off ----

type SignOffDecision string

const (
	SignOffApproved         SignOffDecision = "approved"
	SignOffChangesRequested SignOffDecision = "changes_requested"
)

// SignOff is a person confirming (or sending back) a shipment after reading
// a specific check run. It is the human step the whole workflow ends with.
type SignOff struct {
	ID         string          `json:"id"`
	ShipmentID string          `json:"shipment_id"`
	CheckRunID string          `json:"check_run_id"`
	Decision   SignOffDecision `json:"decision"`
	Reviewer   string          `json:"reviewer"`
	Note       string          `json:"note,omitempty"`
	OpenErrors int             `json:"open_errors"` // error findings in that run at sign-off time
	CreatedAt  time.Time       `json:"created_at"`
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
