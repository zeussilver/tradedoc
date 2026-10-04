package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("test-secret-not-for-production")

// newTestLocal returns a Local in a temp dir, a clock the test can move, and
// a server that mounts the upload handler the way httpapi does.
func newTestLocal(t *testing.T) (*Local, *time.Time, *httptest.Server) {
	t.Helper()
	l := NewLocal(t.TempDir(), testSecret)
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	mux := http.NewServeMux()
	mux.Handle("PUT /v1/local-uploads/{key}", l.UploadHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return l, &now, srv
}

func put(t *testing.T, srv *httptest.Server, rawURL, contentType string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, srv.URL+rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, ""
	}
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("status %d: error body is not the API error shape: %v", resp.StatusCode, err)
	}
	return resp.StatusCode, e.Error.Code
}

// withQuery returns rawURL with one query parameter replaced.
func withQuery(t *testing.T, rawURL, name, value string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestLocalUpload(t *testing.T) {
	const ct = "application/pdf"
	file := []byte("%PDF-1.7 synthetic test file")
	size := int64(len(file))

	tests := []struct {
		name     string
		size     int64                               // size to sign
		mutate   func(t *testing.T, u string) string // tamper with the URL
		ct       string                              // Content-Type header sent
		body     []byte
		advance  time.Duration // move the clock before uploading
		wantCode int
		wantErr  string
		wantFile bool
	}{
		{name: "good signature", size: size, ct: ct, body: file, wantCode: 204, wantFile: true},
		{name: "bad signature", size: size, ct: ct, body: file, wantCode: 403, wantErr: "invalid_signature",
			mutate: func(t *testing.T, u string) string { return withQuery(t, u, "sig", strings.Repeat("ab", 32)) }},
		{name: "size changed in URL", size: size, ct: ct, body: file, wantCode: 403, wantErr: "invalid_signature",
			mutate: func(t *testing.T, u string) string { return withQuery(t, u, "size", "999") }},
		{name: "content type changed in URL", size: size, ct: "image/png", body: file, wantCode: 403, wantErr: "invalid_signature",
			mutate: func(t *testing.T, u string) string { return withQuery(t, u, "ct", "image/png") }},
		{name: "malformed signature", size: size, ct: ct, body: file, wantCode: 403, wantErr: "invalid_signature",
			mutate: func(t *testing.T, u string) string { return withQuery(t, u, "sig", "not-hex") }},
		{name: "expired", size: size, ct: ct, body: file, advance: 6 * time.Minute, wantCode: 403, wantErr: "expired"},
		{name: "wrong content type header", size: size, ct: "image/png", body: file, wantCode: 403, wantErr: "content_type_mismatch"},
		{name: "missing content type header", size: size, ct: "", body: file, wantCode: 403, wantErr: "content_type_mismatch"},
		{name: "body too long", size: size - 1, ct: ct, body: file, wantCode: 413, wantErr: "size_mismatch"},
		{name: "body too short", size: size + 1, ct: ct, body: file, wantCode: 400, wantErr: "size_mismatch"},
		{name: "invalid key", size: size, ct: ct, body: file, wantCode: 404, wantErr: "not_found",
			mutate: func(t *testing.T, u string) string { return strings.Replace(u, "doc_abc123", "DOC..abc", 1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, now, srv := newTestLocal(t)
			up, err := l.PresignPut(context.Background(), "doc_abc123", ct, tt.size, 5*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			u := up.URL
			if tt.mutate != nil {
				u = tt.mutate(t, u)
			}
			*now = now.Add(tt.advance)

			code, errCode := put(t, srv, u, tt.ct, tt.body)
			if code != tt.wantCode || errCode != tt.wantErr {
				t.Fatalf("got %d %q, want %d %q", code, errCode, tt.wantCode, tt.wantErr)
			}
			got, err := l.Get(context.Background(), "doc_abc123", 1<<20)
			if tt.wantFile {
				if err != nil || !bytes.Equal(got, tt.body) {
					t.Fatalf("stored file = %q, %v; want the uploaded bytes", got, err)
				}
			} else if !errors.Is(err, ErrNotFound) {
				// A rejected upload must not leave a partial file behind.
				t.Fatalf("Get after failed upload: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestLocalUploadDuplicateKey(t *testing.T) {
	l, _, srv := newTestLocal(t)
	up, err := l.PresignPut(context.Background(), "doc_dup", "image/png", 3, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := put(t, srv, up.URL, "image/png", []byte("one")); code != 204 {
		t.Fatalf("first upload: %d", code)
	}
	// Reusing the still-valid URL must not overwrite the first file.
	if code, errCode := put(t, srv, up.URL, "image/png", []byte("two")); code != 409 || errCode != "already_uploaded" {
		t.Fatalf("second upload: %d %q, want 409 already_uploaded", code, errCode)
	}
	got, _ := l.Get(context.Background(), "doc_dup", 10)
	if string(got) != "one" {
		t.Fatalf("file = %q, want the first upload", got)
	}
}

func TestLocalPresignShape(t *testing.T) {
	l := NewLocal(t.TempDir(), testSecret)
	up, err := l.PresignPut(context.Background(), "doc_1", "image/jpeg", 42, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if up.Method != http.MethodPut || up.Headers["Content-Type"] != "image/jpeg" {
		t.Fatalf("method %q headers %v", up.Method, up.Headers)
	}
	if !strings.HasPrefix(up.URL, "/v1/local-uploads/doc_1?ct=image%2Fjpeg&size=42&exp=") || !strings.Contains(up.URL, "&sig=") {
		t.Fatalf("unexpected URL %q", up.URL)
	}
	if d := time.Until(up.ExpiresAt); d < 4*time.Minute || d > 5*time.Minute {
		t.Fatalf("ExpiresAt %v is not ~5 minutes away", up.ExpiresAt)
	}
	if _, err := l.PresignPut(context.Background(), "../etc/passwd", "image/jpeg", 1, time.Minute); err == nil {
		t.Fatal("presigning a key with path characters should fail")
	}
}

func TestLocalStore(t *testing.T) {
	ctx := context.Background()
	l := NewLocal(t.TempDir(), testSecret)

	if _, err := l.Size(ctx, "doc_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Size of missing key: %v, want ErrNotFound", err)
	}
	if _, err := l.Get(ctx, "doc_missing", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of missing key: %v, want ErrNotFound", err)
	}

	if err := l.Put(ctx, "doc_a", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if n, err := l.Size(ctx, "doc_a"); err != nil || n != 5 {
		t.Fatalf("Size = %d, %v; want 5", n, err)
	}
	if got, err := l.Get(ctx, "doc_a", 5); err != nil || string(got) != "hello" {
		t.Fatalf("Get with max = size: %q, %v", got, err)
	}
	if _, err := l.Get(ctx, "doc_a", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Get with max < size: %v, want ErrTooLarge", err)
	}

	if err := l.Delete(ctx, "doc_a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, "doc_a"); err != nil {
		t.Fatalf("deleting a missing key should succeed: %v", err)
	}

	for _, bad := range []string{"", "../x", "a/b", "Doc_A", strings.Repeat("a", 65)} {
		if err := l.Put(ctx, bad, strings.NewReader("x")); err == nil {
			t.Errorf("Put(%q) should be rejected", bad)
		}
	}
	// Nothing may have been written outside the upload directory.
	if _, err := os.Stat(filepath.Join(l.dir, "..", "x")); err == nil {
		t.Fatal("a file escaped the upload directory")
	}
}
