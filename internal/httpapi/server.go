// Package httpapi turns HTTP requests into calls on the domain and store.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/zeussilver/tradedoc/internal/ai"
	"github.com/zeussilver/tradedoc/internal/blob"
	"github.com/zeussilver/tradedoc/internal/store"
)

// Config is everything the API needs from outside. cmd/api builds it from
// environment variables; tests build it with in-memory fakes.
type Config struct {
	Store            store.Store
	Blobs            blob.Store
	Extractor        ai.Extractor     // nil: model endpoints answer 503 "model_not_configured"
	Classifier       ai.Classifier    // nil: same
	ModelName        string           // shown by GET /v1/config, e.g. "claude-opus-5-5" or "fake"
	ModelCallsPerDay int64            // global cap via Store.TakeQuota; 0 means default 25
	Static           http.Handler     // SPA handler mounted at "/"; nil: not mounted
	LocalUploads     http.Handler     // mounted at "PUT /v1/local-uploads/{key}"; nil on AWS
	UploadOrigin     string           // added to CSP connect-src, e.g. https://bucket.s3.ap-southeast-2.amazonaws.com
	Now              func() time.Time // nil: time.Now().UTC()
}

// defaultModelCallsPerDay is the global daily cap on model operations. 25 is
// enough for several full demo runs (three operations each) while keeping the
// worst-case daily bill small.
const defaultModelCallsPerDay = 25

// Server holds the dependencies handlers need. Passing them in (rather than
// using globals) lets tests build a Server with a fresh in-memory store.
type Server struct {
	store        store.Store
	blobs        blob.Store
	extractor    ai.Extractor
	classifier   ai.Classifier
	modelName    string
	modelQuota   int64
	static       http.Handler
	localUploads http.Handler
	csp          string
	now          func() time.Time

	// In-memory buckets, per client IP and per server instance (see ratelimit.go).
	apiLimit   *rateLimiter // every /v1/ request
	modelLimit *rateLimiter // the two routes that call the paid model
}

func New(cfg Config) *Server {
	s := &Server{
		store:        cfg.Store,
		blobs:        cfg.Blobs,
		extractor:    cfg.Extractor,
		classifier:   cfg.Classifier,
		modelName:    cfg.ModelName,
		modelQuota:   cfg.ModelCallsPerDay,
		static:       cfg.Static,
		localUploads: cfg.LocalUploads,
		csp:          contentSecurityPolicy(cfg.UploadOrigin),
		now:          cfg.Now,
		apiLimit:     newRateLimiter(apiRate, apiBurst),
		modelLimit:   newRateLimiter(modelRate, modelBurst),
	}
	if s.modelQuota <= 0 {
		s.modelQuota = defaultModelCallsPerDay
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s
}

// Handler registers the routes. Since Go 1.22 the standard ServeMux matches
// methods and path parameters ("GET /v1/shipments/{id}"), so no router
// library is needed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/config", s.getConfig)

	mux.HandleFunc("POST /v1/shipments", s.createShipment)
	mux.HandleFunc("GET /v1/shipments/{id}", s.getShipment)

	// The browser uploads straight to storage: ask for a URL, PUT the file,
	// then tell us it is done so we can check what actually arrived.
	mux.HandleFunc("POST /v1/shipments/{id}/uploads", s.startUpload)
	mux.HandleFunc("POST /v1/documents/{id}/complete", s.completeUpload)
	mux.HandleFunc("GET /v1/shipments/{id}/documents", s.listDocuments)

	mux.HandleFunc("POST /v1/documents/{id}/extractions", s.submitExtraction)
	mux.HandleFunc("GET /v1/documents/{id}/extractions", s.listExtractions)
	mux.Handle("POST /v1/documents/{id}/extract", s.withModelLimit(http.HandlerFunc(s.extractWithModel)))

	mux.HandleFunc("POST /v1/shipments/{id}/checks", s.runChecks)
	mux.HandleFunc("GET /v1/shipments/{id}/checks", s.listCheckRuns)

	mux.Handle("POST /v1/shipments/{id}/suggestions", s.withModelLimit(http.HandlerFunc(s.createSuggestions)))
	mux.HandleFunc("GET /v1/shipments/{id}/suggestions", s.listSuggestions)
	mux.HandleFunc("POST /v1/suggestions/{id}/review", s.reviewSuggestion)

	mux.HandleFunc("POST /v1/shipments/{id}/signoffs", s.createSignOff)
	mux.HandleFunc("GET /v1/shipments/{id}/signoffs", s.listSignOffs)

	mux.HandleFunc("GET /v1/shipments/{id}/audit", s.listAudit)

	if s.localUploads != nil {
		// Local development only. This body is the raw file, not JSON, so the
		// JSON size limit doesn't apply; cap it at the largest file we accept.
		mux.Handle("PUT /v1/local-uploads/{key}", http.MaxBytesHandler(s.localUploads, maxUploadBytes))
		// The older multipart upload, where the file passes through this
		// server. Only local: a Lambda Function URL refuses request bodies
		// over 6 MB (and base64-encodes binary ones first), so on AWS a 10 MiB
		// upload could never arrive. The browser uploads straight to S3 there.
		mux.HandleFunc("POST /v1/shipments/{id}/documents", s.uploadDocument)
	}

	// "/" is the least specific pattern, so it only gets requests no route
	// above matched.
	mux.HandleFunc("/", s.fallback)

	// Outermost runs first:
	// request ID -> recover -> logging -> security headers -> rate limit -> route.
	var h http.Handler = mux
	h = s.withAPIRateLimit(h)
	h = withSecurityHeaders(s.csp, h)
	h = withLogging(h)
	h = withRecover(h)
	h = withRequestID(h)
	return h
}

// fallback serves the single-page app for browser paths. Unknown API paths
// always get the JSON error shape, so the frontend never receives the app's
// index.html where it expected JSON.
func (s *Server) fallback(w http.ResponseWriter, r *http.Request) {
	isAPI := r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/")
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead
	if s.static != nil && !isAPI && isRead {
		s.static.ServeHTTP(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "no such route", nil)
}

// newID returns e.g. "shp_3f9a...". 80 random bits make IDs unguessable,
// which matters because there is no login: knowing a shipment's ID is what
// lets you see it.
func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
