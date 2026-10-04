// Package store persists domain records. Handlers depend on the Store
// interface, not on a concrete database, so the in-memory version used now
// can be swapped for Postgres or DynamoDB later without touching them.
package store

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/zeussilver/tradedoc/internal/domain"
)

var ErrNotFound = errors.New("not found")

type Store interface {
	CreateShipment(ctx context.Context, s domain.Shipment) error
	GetShipment(ctx context.Context, id string) (domain.Shipment, error)

	CreateDocument(ctx context.Context, d domain.Document) error
	GetDocument(ctx context.Context, id string) (domain.Document, error)
	ListDocuments(ctx context.Context, shipmentID string) ([]domain.Document, error)
	UpdateDocumentStatus(ctx context.Context, id string, status domain.DocStatus) error

	AddExtraction(ctx context.Context, e domain.Extraction) error
	ListExtractions(ctx context.Context, documentID string) ([]domain.Extraction, error)

	AppendAudit(ctx context.Context, e domain.AuditEvent) error
	ListAudit(ctx context.Context, shipmentID string) ([]domain.AuditEvent, error)
}

// Memory is a thread-safe in-memory Store. HTTP handlers run on many
// goroutines at once, so every map access goes through the mutex.
type Memory struct {
	mu          sync.RWMutex
	shipments   map[string]domain.Shipment
	documents   map[string]domain.Document
	extractions map[string][]domain.Extraction // by document ID
	audit       []domain.AuditEvent
}

func NewMemory() *Memory {
	return &Memory{
		shipments:   map[string]domain.Shipment{},
		documents:   map[string]domain.Document{},
		extractions: map[string][]domain.Extraction{},
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

func (m *Memory) UpdateDocumentStatus(_ context.Context, id string, status domain.DocStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.documents[id]
	if !ok {
		return ErrNotFound
	}
	d.Status = status
	m.documents[id] = d
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

func (m *Memory) AppendAudit(_ context.Context, e domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	return nil
}

func (m *Memory) ListAudit(_ context.Context, shipmentID string) ([]domain.AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.AuditEvent{}
	for _, e := range m.audit {
		if e.ShipmentID == shipmentID {
			out = append(out, e)
		}
	}
	return out, nil
}
