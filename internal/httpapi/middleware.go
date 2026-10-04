package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// A middleware takes a handler and returns a handler that wraps it.
// Chaining them gives every request the same logging, IDs and panic safety.

type ctxKey int

const requestIDKey ctxKey = 0

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newID("req")
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// statusRecorder remembers the status code so the logger can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withLogging logs method, route, status and duration: never headers or
// bodies, which may hold document contents or API keys.
//
// It logs the route pattern ("POST /v1/shipments/{id}/checks"), not the URL
// path. There is no login, so a shipment or document ID in the path is the
// only thing needed to open that data: writing it to the logs would let
// anyone who can read them open every shipment. The request ID ties a log
// line to the audit trail instead. The ServeMux fills in r.Pattern on this
// same request while routing, so it is set by the time next returns ("/" for
// unknown paths, which the fallback route catches).
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"route", r.Pattern,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withRecover turns a panic in a handler into a 500 instead of killing the server.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "request_id", requestIDFrom(r.Context()), "panic", v)
				writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy tells the browser where the page may load things
// from. Only our own origin, plus the storage origin the browser PUTs
// uploads to (S3 on AWS; empty locally, where uploads go to this server).
// If an attacker ever got script into the page, it couldn't send data
// anywhere else.
func contentSecurityPolicy(uploadOrigin string) string {
	connect := "'self'"
	if uploadOrigin != "" {
		connect += " " + uploadOrigin
	}
	return "default-src 'self'; connect-src " + connect +
		"; img-src 'self' data: blob:; style-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'"
}

// withSecurityHeaders sets browser protections on every response, API and
// static files alike.
func withSecurityHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")             // don't guess types: a .pdf stays a PDF
		h.Set("Referrer-Policy", "no-referrer")                // don't leak shipment IDs in URLs to other sites
		h.Set("X-Frame-Options", "DENY")                       // no clickjacking via iframes (older browsers)
		h.Set("Strict-Transport-Security", "max-age=31536000") // HTTPS only for a year once seen over HTTPS
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
