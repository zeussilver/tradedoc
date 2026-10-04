package blob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Keys are IDs the server generates (e.g. "doc_3f9a..."), never user input.
// Allowing only lowercase letters, digits and "_" means a key can never
// contain "/" or "..", so it can't point outside the upload directory or
// the S3 prefix (path traversal).
var reKey = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func checkKey(key string) error {
	if !reKey.MatchString(key) {
		return fmt.Errorf("invalid blob key %q", key)
	}
	return nil
}

// Local stores files in a directory on disk. It is for local development
// only; on AWS the same interface is implemented by S3.
type Local struct {
	dir    string
	secret []byte           // HMAC key that signs upload URLs
	now    func() time.Time // replaceable in tests to check expiry
}

func NewLocal(dir string, secret []byte) *Local {
	return &Local{dir: dir, secret: secret, now: time.Now}
}

func (l *Local) path(key string) string { return filepath.Join(l.dir, key) }

func (l *Local) Put(_ context.Context, key string, r io.Reader) error {
	if err := checkKey(key); err != nil {
		return err
	}
	// 0700/0600: uploaded trade documents are readable by this user only.
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path(key), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(l.path(key)) // don't leave half a file behind
		return err
	}
	return f.Close()
}

func (l *Local) Get(_ context.Context, key string, max int64) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	f, err := os.Open(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Read at most one byte more than allowed: if we get that extra byte the
	// file is too large, and we never hold more than max+1 bytes in memory.
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

func (l *Local) Size(_ context.Context, key string) (int64, error) {
	if err := checkKey(key); err != nil {
		return 0, err
	}
	fi, err := os.Stat(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Delete succeeds if the file is already gone, like S3's DeleteObject.
func (l *Local) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	err := os.Remove(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// PresignPut returns a signed URL on this server that behaves like an S3
// presigned PUT: the URL carries the content type, exact size and expiry,
// plus an HMAC signature over them, so nobody can change any of them
// without the server noticing. The frontend therefore uses one upload flow
// (ask the API for a URL, PUT the file to it, call /complete) both locally
// and on AWS.
func (l *Local) PresignPut(_ context.Context, key, contentType string, size int64, ttl time.Duration) (PresignedUpload, error) {
	if err := checkKey(key); err != nil {
		return PresignedUpload{}, err
	}
	if size < 0 {
		return PresignedUpload{}, fmt.Errorf("invalid size %d", size)
	}
	exp := l.now().Add(ttl).Unix()
	u := "/v1/local-uploads/" + key +
		"?ct=" + url.QueryEscape(contentType) +
		"&size=" + strconv.FormatInt(size, 10) +
		"&exp=" + strconv.FormatInt(exp, 10) +
		"&sig=" + hex.EncodeToString(l.sign(key, contentType, size, exp))
	return PresignedUpload{
		URL:       u,
		Method:    http.MethodPut,
		Headers:   map[string]string{"Content-Type": contentType},
		ExpiresAt: time.Unix(exp, 0).UTC(),
	}, nil
}

// sign is HMAC-SHA256 over "key|content type|size|expiry". Splitting it back
// apart is unambiguous: the key can't contain "|" and the last two fields
// are plain numbers.
func (l *Local) sign(key, contentType string, size, exp int64) []byte {
	mac := hmac.New(sha256.New, l.secret)
	fmt.Fprintf(mac, "%s|%s|%d|%d", key, contentType, size, exp)
	return mac.Sum(nil)
}

// UploadHandler serves PUT /v1/local-uploads/{key}: the local stand-in for
// the browser uploading straight to S3. It must be mounted on a ServeMux
// pattern with a {key} wildcard.
func (l *Local) UploadHandler() http.Handler {
	return http.HandlerFunc(l.handleUpload)
}

func (l *Local) handleUpload(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if checkKey(key) != nil {
		writeUploadError(w, http.StatusNotFound, "not_found", "unknown upload key")
		return
	}

	// Everything the signature covers comes from the URL. A value we can't
	// parse can't have been signed by us, so it's treated as a bad signature.
	q := r.URL.Query()
	contentType := q.Get("ct")
	size, errSize := strconv.ParseInt(q.Get("size"), 10, 64)
	exp, errExp := strconv.ParseInt(q.Get("exp"), 10, 64)
	sig, errSig := hex.DecodeString(q.Get("sig"))
	if errSize != nil || errExp != nil || errSig != nil || size < 0 {
		writeUploadError(w, http.StatusForbidden, "invalid_signature", "upload URL is malformed")
		return
	}
	// hmac.Equal takes the same time whether the first or the last byte
	// differs, so an attacker can't guess a signature byte by byte from
	// response times.
	if !hmac.Equal(sig, l.sign(key, contentType, size, exp)) {
		writeUploadError(w, http.StatusForbidden, "invalid_signature", "upload URL signature does not match")
		return
	}
	if l.now().Unix() > exp {
		writeUploadError(w, http.StatusForbidden, "expired", "upload URL has expired; request a new one")
		return
	}
	// S3 rejects a PUT whose signed Content-Type header differs; so do we.
	if r.Header.Get("Content-Type") != contentType {
		writeUploadError(w, http.StatusForbidden, "content_type_mismatch", "Content-Type header must match the signed upload")
		return
	}

	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		slog.Error("local upload: create dir", "err", err)
		writeUploadError(w, http.StatusInternalServerError, "internal", "could not store file")
		return
	}
	// O_EXCL: creating the file fails if it already exists, so a signed URL
	// can't be reused to overwrite a file after it was uploaded and checked.
	f, err := os.OpenFile(l.path(key), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		writeUploadError(w, http.StatusConflict, "already_uploaded", "a file was already uploaded with this URL")
		return
	}
	if err != nil {
		slog.Error("local upload: create file", "err", err)
		writeUploadError(w, http.StatusInternalServerError, "internal", "could not store file")
		return
	}

	// MaxBytesReader stops reading after the signed size, so a client can't
	// fill the disk by sending more than it declared.
	n, copyErr := io.Copy(f, http.MaxBytesReader(w, r.Body, size))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n != size {
		os.Remove(l.path(key)) // a failed upload leaves nothing behind, so the URL can be retried
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(copyErr, &tooBig):
			writeUploadError(w, http.StatusRequestEntityTooLarge, "size_mismatch", "body is larger than the signed size")
		case copyErr == nil && closeErr == nil:
			writeUploadError(w, http.StatusBadRequest, "size_mismatch", "body is smaller than the signed size")
		default:
			writeUploadError(w, http.StatusBadRequest, "upload_failed", "could not read the request body")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeUploadError uses the API's error shape {"error": {"code", "message"}}.
// It is a small copy of httpapi's helper because httpapi imports this package
// (importing it back would be a cycle).
func writeUploadError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]map[string]string{"error": {"code": code, "message": msg}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("local upload: encode error", "err", err)
	}
}
