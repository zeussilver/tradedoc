// Package blob stores uploaded files: on local disk for development, in S3
// on AWS. Both implement Store, so handlers don't know which one they use.
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound = errors.New("blob not found")
	ErrTooLarge = errors.New("blob larger than allowed")
)

type Store interface {
	// Put writes a file the server already holds (used by the multipart
	// upload endpoint and tests).
	Put(ctx context.Context, key string, r io.Reader) error
	// Get reads a whole file, failing with ErrTooLarge rather than reading
	// more than max bytes.
	Get(ctx context.Context, key string, max int64) ([]byte, error)
	// Size returns the stored size in bytes, or ErrNotFound.
	Size(ctx context.Context, key string) (int64, error)
	Delete(ctx context.Context, key string) error
	// PresignPut returns a short-lived URL the browser can PUT the file to
	// directly, so file bytes never pass through the API server.
	PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (PresignedUpload, error)
}

// PresignedUpload tells the client exactly how to send the file.
// Headers must be sent as given: they are part of the signature.
type PresignedUpload struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"` // always PUT
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}
