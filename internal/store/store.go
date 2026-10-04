// Package store persists domain records. Handlers depend on the Store
// interface, not on a concrete database, so the in-memory version used in
// tests and local runs and the DynamoDB version used on AWS are swappable.
package store

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/zeussilver/tradedoc/internal/domain"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrConflict means the change is not allowed in the record's current
	// state, e.g. reviewing a suggestion that was already reviewed.
	ErrConflict = errors.New("conflict")
	// ErrLineAccepted means another candidate for the same invoice line (same
	// extraction, same line_no) is already accepted. A line has exactly one
	// classification, so a second acceptance is refused.
	ErrLineAccepted = errors.New("another candidate for this line is already accepted")
	// ErrTooLarge means the record is too big for the backing database
	// (DynamoDB items are limited to 400 KB).
	ErrTooLarge = errors.New("record too large to store")
)

// Store lists are returned oldest first.
type Store interface {
	CreateShipment(ctx context.Context, s domain.Shipment) error
	GetShipment(ctx context.Context, id string) (domain.Shipment, error)

	CreateDocument(ctx context.Context, d domain.Document) error
	GetDocument(ctx context.Context, id string) (domain.Document, error)
	ListDocuments(ctx context.Context, shipmentID string) ([]domain.Document, error)
	// UpdateDocument replaces a document's metadata (status, size, hash...).
	UpdateDocument(ctx context.Context, d domain.Document) error

	AddExtraction(ctx context.Context, e domain.Extraction) error
	ListExtractions(ctx context.Context, documentID string) ([]domain.Extraction, error)

	AddCheckRun(ctx context.Context, r domain.CheckRun) error
	ListCheckRuns(ctx context.Context, shipmentID string) ([]domain.CheckRun, error)

	AddSuggestions(ctx context.Context, s []domain.ClassificationSuggestion) error
	ListSuggestions(ctx context.Context, shipmentID string) ([]domain.ClassificationSuggestion, error)
	GetSuggestion(ctx context.Context, id string) (domain.ClassificationSuggestion, error)
	// ReviewSuggestion records a decision on a pending suggestion and returns
	// the updated record. It returns ErrConflict if the suggestion is not
	// pending, so two reviewers can't both decide the same suggestion, and
	// ErrLineAccepted when accepting while a sibling candidate (same
	// ExtractionID and LineNo) is already accepted. Both checks are atomic.
	// On error the returned record must not be used.
	ReviewSuggestion(ctx context.Context, id string, r domain.Review) (domain.ClassificationSuggestion, error)

	AddSignOff(ctx context.Context, s domain.SignOff) error
	ListSignOffs(ctx context.Context, shipmentID string) ([]domain.SignOff, error)

	AppendAudit(ctx context.Context, e domain.AuditEvent) error
	ListAudit(ctx context.Context, shipmentID string) ([]domain.AuditEvent, error)

	// TakeQuota uses one unit of the named counter if it is below limit and
	// reports whether it did. It caps paid model calls per day across all
	// server instances (key e.g. "model_calls:2026-10-04").
	TakeQuota(ctx context.Context, key string, limit int64) (bool, error)
}

// Memory is a thread-safe in-memory Store. HTTP handlers run on many
// goroutines at once, so every map access goes through the mutex.
type Memory struct {
	mu          sync.RWMutex
	shipments   map[string]domain.Shipment
	documents   map[string]domain.Document
	extractions map[string][]domain.Extraction // by document ID
	checkRuns   []domain.CheckRun
	suggestions []domain.ClassificationSuggestion
	signOffs    []domain.SignOff
	audit       []domain.AuditEvent
	quotas      map[string]int64
}

func NewMemory() *Memory {
	return &Memory{
		shipments:   map[string]domain.Shipment{},
		documents:   map[string]domain.Document{},
		extractions: map[string][]domain.Extraction{},
		quotas:      map[string]int64{},
	}
}

func (m *Memory) CreateShipment(_ context.Context, s domain.Shipment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shipments[s.ID] = s
	return nil
}

func (m *Memory) GetShipment(_ context.Context, id string) (domain.Shipment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.shipments[id]
	if !ok {
		return domain.Shipment{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) CreateDocument(_ context.Context, d domain.Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.documents[d.ID] = d
	return nil
}

func (m *Memory) GetDocument(_ context.Context, id string) (domain.Document, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.documents[id]
	if !ok {
		return domain.Document{}, ErrNotFound
	}
	return d, nil
}

func (m *Memory) ListDocuments(_ context.Context, shipmentID string) ([]domain.Document, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.Document{}
	for _, d := range m.documents {
		if d.ShipmentID == shipmentID {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadedAt.Before(out[j].UploadedAt) })
	return out, nil
}

func (m *Memory) UpdateDocument(_ context.Context, d domain.Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.documents[d.ID]; !ok {
		return ErrNotFound
	}
	m.documents[d.ID] = d
	return nil
}

func (m *Memory) AddExtraction(_ context.Context, e domain.Extraction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.extractions[e.DocumentID] = append(m.extractions[e.DocumentID], e)
	return nil
}

func (m *Memory) ListExtractions(_ context.Context, documentID string) ([]domain.Extraction, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]domain.Extraction{}, m.extractions[documentID]...), nil
}

func (m *Memory) AddCheckRun(_ context.Context, r domain.CheckRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkRuns = append(m.checkRuns, r)
	return nil
}

func (m *Memory) ListCheckRuns(_ context.Context, shipmentID string) ([]domain.CheckRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return filter(m.checkRuns, func(r domain.CheckRun) bool { return r.ShipmentID == shipmentID }), nil
}

func (m *Memory) AddSuggestions(_ context.Context, s []domain.ClassificationSuggestion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suggestions = append(m.suggestions, s...)
	return nil
}

func (m *Memory) ListSuggestions(_ context.Context, shipmentID string) ([]domain.ClassificationSuggestion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return filter(m.suggestions, func(s domain.ClassificationSuggestion) bool { return s.ShipmentID == shipmentID }), nil
}

func (m *Memory) GetSuggestion(_ context.Context, id string) (domain.ClassificationSuggestion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.suggestions {
		if s.ID == id {
			return s, nil
		}
	}
	return domain.ClassificationSuggestion{}, ErrNotFound
}

func (m *Memory) ReviewSuggestion(_ context.Context, id string, r domain.Review) (domain.ClassificationSuggestion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.suggestions {
		if s.ID != id {
			continue
		}
		if s.Status != domain.SuggestionPending {
			return s, ErrConflict
		}
		if r.Status == domain.SuggestionAccepted {
			for _, o := range m.suggestions {
				if o.ExtractionID == s.ExtractionID && o.LineNo == s.LineNo && o.Status == domain.SuggestionAccepted {
					return s, ErrLineAccepted
				}
			}
		}
		at := r.At
		s.Status, s.ReviewedBy, s.ReviewNote, s.ReviewedAt = r.Status, r.Reviewer, r.Note, &at
		m.suggestions[i] = s
		return s, nil
	}
	return domain.ClassificationSuggestion{}, ErrNotFound
}

func (m *Memory) AddSignOff(_ context.Context, s domain.SignOff) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signOffs = append(m.signOffs, s)
	return nil
}

func (m *Memory) ListSignOffs(_ context.Context, shipmentID string) ([]domain.SignOff, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return filter(m.signOffs, func(s domain.SignOff) bool { return s.ShipmentID == shipmentID }), nil
}

func (m *Memory) AppendAudit(_ context.Context, e domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	return nil
}

func (m *Memory) ListAudit(_ context.Context, shipmentID string) ([]domain.AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return filter(m.audit, func(e domain.AuditEvent) bool { return e.ShipmentID == shipmentID }), nil
}

func (m *Memory) TakeQuota(_ context.Context, key string, limit int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quotas[key] >= limit {
		return false, nil
	}
	m.quotas[key]++
	return true, nil
}

// filter returns the matching items in their original (insertion) order,
// as a new slice so callers can't modify the store's copy.
func filter[T any](items []T, keep func(T) bool) []T {
	out := []T{}
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	return out
}
