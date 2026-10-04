package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/zeussilver/tradedoc/internal/store"
)

func TestRateLimiterBucket(t *testing.T) {
	l := newRateLimiter(rate.Every(30*time.Second), 4)
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	for i := range 4 {
		if ok, _ := l.allow("a", t0); !ok {
			t.Fatalf("request %d within the burst was refused", i+1)
		}
	}
	ok, wait := l.allow("a", t0)
	if ok || wait != 30*time.Second {
		t.Fatalf("5th request: ok=%v wait=%v, want refused with 30s", ok, wait)
	}
	if ok, _ := l.allow("b", t0); !ok {
		t.Fatal("another client must have its own bucket")
	}
	if ok, _ := l.allow("a", t0.Add(30*time.Second)); !ok {
		t.Fatal("one token should have refilled after 30s")
	}
}

func TestRateLimiterForgetsIdleClients(t *testing.T) {
	l := newRateLimiter(rate.Every(30*time.Second), 4)
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	l.allow("a", t0)
	l.allow("b", t0.Add(idleTTL+sweepEvery))
	if n := l.size(); n != 1 {
		t.Fatalf("tracking %d clients, want 1 (idle one forgotten)", n)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct{ remoteAddr, want string }{
		{"203.0.113.7:51234", "203.0.113.7"},
		{"203.0.113.7", "203.0.113.7"}, // Lambda Function URLs give a bare IP
		{"[2001:db8:1:2:3:4:5:6]:443", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"}, // same /64 as above: same bucket
		{"[::ffff:203.0.113.7]:80", "203.0.113.7"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tt := range tests {
		if got := clientIP(tt.remoteAddr); got != tt.want {
			t.Errorf("clientIP(%q) = %q, want %q", tt.remoteAddr, got, tt.want)
		}
	}
}

// newLimitedHandler builds the real handler with the real limiters, and no
// model, so model routes answer 503 when the limiter lets them through.
func newLimitedHandler(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := New(Config{Store: store.NewMemory(), Blobs: newMemBlobs()})
	return srv, srv.Handler()
}

func serve(h http.Handler, method, path, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestModelRouteRateLimit(t *testing.T) {
	_, h := newLimitedHandler(t)
	for i := range modelBurst {
		if rec := serve(h, "POST", "/v1/documents/doc_x/extract", "198.51.100.1:1000"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("call %d: want 503 from the handler, got %d", i+1, rec.Code)
		}
	}
	// The suggestions route shares the same strict bucket.
	rec := serve(h, "POST", "/v1/shipments/shp_x/suggestions", "198.51.100.1:2000")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("want 429 rate_limited, got %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	// A different client is unaffected.
	if rec := serve(h, "POST", "/v1/documents/doc_x/extract", "198.51.100.2:1000"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("other client: got %d", rec.Code)
	}
}

func TestAPIRateLimit(t *testing.T) {
	_, h := newLimitedHandler(t)
	const addr = "198.51.100.9:4000"
	limited := false
	// The burst is 30; refilling at 5/s, 200 requests can't all get through
	// unless they take over 30 s.
	for range 200 {
		rec := serve(h, "GET", "/v1/config", addr)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			if rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
			}
			break
		}
	}
	if !limited {
		t.Fatal("never rate limited")
	}
	// Health checks and static files are not limited.
	if rec := serve(h, "GET", "/healthz", addr); rec.Code != http.StatusOK {
		t.Fatalf("/healthz got %d", rec.Code)
	}
}

func TestRemoteAddrWithoutPort(t *testing.T) {
	srv, h := newLimitedHandler(t)
	rec := serve(h, "GET", "/v1/config", "203.0.113.7") // what lambdaurl sets
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if srv.apiLimit.clients["203.0.113.7"] == nil {
		t.Fatal("bare-IP client was not tracked under its IP")
	}
}

func TestSecurityHeaders(t *testing.T) {
	const origin = "https://tradedoc-uploads.s3.ap-southeast-2.amazonaws.com"
	srv := New(Config{Store: store.NewMemory(), Blobs: newMemBlobs(), UploadOrigin: origin})
	h := srv.Handler()
	for _, path := range []string{"/healthz", "/v1/shipments/shp_missing"} {
		rec := serve(h, "GET", path, "192.0.2.1:1")
		want := map[string]string{
			"X-Content-Type-Options":    "nosniff",
			"Referrer-Policy":           "no-referrer",
			"X-Frame-Options":           "DENY",
			"Strict-Transport-Security": "max-age=31536000",
		}
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s %s = %q, want %q", path, k, got, v)
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, part := range []string{"default-src 'self'", "connect-src 'self' " + origin + ";", "object-src 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, part) {
				t.Errorf("%s CSP %q lacks %q", path, csp, part)
			}
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s has no request ID", path)
		}
	}
	if csp := contentSecurityPolicy(""); !strings.Contains(csp, "connect-src 'self';") {
		t.Errorf("CSP without upload origin = %q", csp)
	}
}

func TestStaticAndUnknownAPIRoutes(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<!doctype html>spa") })
	e := newEnv(t, func(c *Config) { c.Static = spa })

	for _, path := range []string{"/", "/shipments/shp_123", "/assets/app.js"} {
		r := e.mustDo("GET", path, nil, http.StatusOK)
		if !bytes.Contains(r.raw, []byte("spa")) {
			t.Fatalf("GET %s = %s, want the SPA", path, r.raw)
		}
	}
	// Unknown API paths get the JSON error shape, never the SPA.
	for _, req := range [][2]string{{"GET", "/v1/nope"}, {"POST", "/v1/nope"}, {"GET", "/v1/shipments/x/nope"}} {
		r := e.mustDo(req[0], req[1], nil, http.StatusNotFound)
		if r.errCode(t) != "not_found" || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s %s = %s", req[0], req[1], r.raw)
		}
	}
	// Real API routes still win over the SPA.
	if r := e.mustDo("GET", "/v1/config", nil, http.StatusOK); bytes.Contains(r.raw, []byte("spa")) {
		t.Fatal("/v1/config served the SPA")
	}
}

func TestLocalUploadsMount(t *testing.T) {
	var gotKey string
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.PathValue("key")
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	e := newEnv(t, func(c *Config) { c.LocalUploads = local })

	put := func(body []byte) response {
		req, _ := http.NewRequest(http.MethodPut, e.url+"/v1/local-uploads/doc_123?expires=1&sig=abc", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/pdf")
		return e.send(req)
	}
	if r := put(fakePDF); r.code != http.StatusNoContent || gotKey != "doc_123" {
		t.Fatalf("got %d, key %q", r.code, gotKey)
	}
	// The raw upload isn't held to the 1 MiB JSON limit, but is capped at 10 MiB.
	if r := put(make([]byte, 2<<20)); r.code != http.StatusNoContent {
		t.Fatalf("2 MiB upload: got %d", r.code)
	}
	if r := put(make([]byte, 10<<20+1)); r.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over 10 MiB: got %d", r.code)
	}

	// Without LocalUploads (on AWS) the route doesn't exist.
	e2 := newEnv(t)
	req, _ := http.NewRequest(http.MethodPut, e2.url+"/v1/local-uploads/doc_123", bytes.NewReader(fakePDF))
	if r := e2.send(req); r.code != http.StatusNotFound {
		t.Fatalf("without LocalUploads: got %d", r.code)
	}
}

// The request log names the route pattern, never the URL path: with no
// login, a shipment ID in a path is all it takes to open that shipment, so
// it must not end up in the logs.
func TestRequestLogShowsRouteNotPath(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	_, h := newLimitedHandler(t)
	req := httptest.NewRequest("POST", "/v1/shipments", strings.NewReader(`{"reference":"JOB-040"}`))
	req.RemoteAddr = "192.0.2.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var sh struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &sh); err != nil || sh.ID == "" {
		t.Fatalf("create shipment: %d %s", rec.Code, rec.Body)
	}
	logs.Reset() // only the requests below, which carry the ID in their path

	serve(h, "GET", "/v1/shipments/"+sh.ID+"/checks", "192.0.2.1:1")
	serve(h, "GET", "/v1/shipments/"+sh.ID+"/nope", "192.0.2.1:1")
	serve(h, "GET", "/v1/local-uploads/"+sh.ID+"?sig=secret", "192.0.2.1:1")

	if bytes.Contains(logs.Bytes(), []byte(sh.ID)) || bytes.Contains(logs.Bytes(), []byte("secret")) {
		t.Fatalf("log contains an ID or signature:\n%s", logs.String())
	}
	var routes []string
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if _, ok := entry["path"]; ok {
			t.Fatalf("log line has a path: %s", line)
		}
		routes = append(routes, fmt.Sprint(entry["route"]))
	}
	// Unknown paths fall through to the "/" route.
	want := "GET /v1/shipments/{id}/checks|/|/"
	if got := strings.Join(routes, "|"); got != want {
		t.Fatalf("routes = %s, want %s", got, want)
	}
}
