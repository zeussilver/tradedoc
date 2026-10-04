// Package httpapi turns HTTP requests into calls on the domain and store.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/zeussilver/tradedoc/internal/blob"
	"github.com/zeussilver/tradedoc/internal/store"
)

// Server holds the dependencies handlers need. Passing them in (rather than
// using globals) lets tests build a Server with a fresh in-memory store.
type Server struct {
	store store.Store
	blobs blob.Store
	now   func() time.Time
}

func New(st store.Store, bl blob.Store) *Server {
	return &Server{store: st, blobs: bl, now: func() time.Time { return time.Now().UTC() }}
}

// Handler registers the routes. Since Go 1.22 the standard ServeMux matches
// methods and path parameters ("GET /v1/shipments/{id}"), so no router
// library is needed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)

	mux.HandleFunc("POST /v1/shipments", s.createShipment)
	mux.HandleFunc("GET /v1/shipments/{id}", s.getShipment)

	mux.HandleFunc("POST /v1/shipments/{id}/documents", s.uploadDocument)
	mux.HandleFunc("GET /v1/shipments/{id}/documents", s.listDocuments)

	mux.HandleFunc("POST /v1/documents/{id}/extractions", s.submitExtraction)
	mux.HandleFunc("GET /v1/documents/{id}/extractions", s.listExtractions)

	mux.HandleFunc("GET /v1/shipments/{id}/audit", s.listAudit)

	// Outermost runs first: request ID -> recover -> logging -> route.
	return withRequestID(withRecover(withLogging(mux)))
}

func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
