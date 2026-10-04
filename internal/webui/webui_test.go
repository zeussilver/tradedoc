package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// built looks like a Vite build: index.html, a hashed asset and a sample.
var built = fstest.MapFS{
	"index.html":           {Data: []byte("<!doctype html><title>TradeDoc Check</title>")},
	"assets/index-abc1.js": {Data: []byte("console.log(1)")},
	"samples/a.pdf":        {Data: []byte("%PDF-1.4")},
	".gitkeep":             {Data: nil},
}

// unbuilt is a fresh clone: only the placeholder that keeps go:embed happy.
var unbuilt = fstest.MapFS{
	".gitkeep": {Data: nil},
}

func TestHandler(t *testing.T) {
	tests := []struct {
		name       string
		files      fstest.MapFS
		method     string
		path       string
		wantStatus int
		wantBody   string // substring
		wantCache  string // exact Cache-Control, "" = don't check
		wantType   string // Content-Type prefix, "" = don't check
	}{
		{"index at root", built, "GET", "/", 200, "TradeDoc Check", "no-cache", "text/html"},
		{"hashed asset cached for a year", built, "GET", "/assets/index-abc1.js", 200, "console.log", "public, max-age=31536000, immutable", "text/javascript"},
		{"other files revalidate", built, "GET", "/samples/a.pdf", 200, "%PDF", "no-cache", "application/pdf"},
		{"SPA fallback for an app path", built, "GET", "/shipments/shp_123", 200, "TradeDoc Check", "no-cache", "text/html"},
		{"missing asset is a real 404", built, "GET", "/assets/missing.js", 404, "", "", ""},
		{"missing file with extension is 404", built, "GET", "/favicon.ico", 404, "", "", ""},
		{"directory is not listed", built, "GET", "/assets", 200, "TradeDoc Check", "no-cache", "text/html"},
		{"dotfiles are not served", built, "GET", "/.gitkeep", 404, "", "", ""},
		{"HEAD works", built, "HEAD", "/", 200, "", "no-cache", "text/html"},
		{"POST is refused", built, "POST", "/", 405, "", "", ""},
		{"not built: placeholder at root", unbuilt, "GET", "/", 200, "make web", "no-cache", "text/plain"},
		{"not built: placeholder for app paths", unbuilt, "GET", "/shipments/shp_123", 200, "make web", "", "text/plain"},
		{"not built: assets still 404", unbuilt, "GET", "/assets/index-abc1.js", 404, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newHandler(tt.files).ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantCache != "" && rec.Header().Get("Cache-Control") != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tt.wantCache)
			}
			if tt.wantType != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.wantType) {
				t.Errorf("Content-Type = %q, want prefix %q", rec.Header().Get("Content-Type"), tt.wantType)
			}
		})
	}
}

// TestEmbeddedHandler checks the real embedded files: whether or not the
// frontend has been built, the root page must answer 200.
func TestEmbeddedHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestEmbeddedIndexFollowsCSP checks the built page against the server's
// Content-Security-Policy, which allows scripts and styles from our own
// files only ('self'). An inline <script>, a <style> block or a style
// attribute would work in `npm run dev` but be blocked in production.
func TestEmbeddedIndexFollowsCSP(t *testing.T) {
	page, err := fs.ReadFile(dist, "dist/index.html")
	if err != nil {
		t.Skip("frontend not built: nothing to check")
	}
	html := string(page)
	for _, tag := range regexp.MustCompile(`<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(tag, " src=") {
			t.Errorf("inline script %q: the CSP blocks it", tag)
		}
	}
	if strings.Contains(html, "<style") {
		t.Error("<style> block: the CSP blocks it")
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(html) {
		t.Error("style attribute: the CSP blocks it")
	}
}
