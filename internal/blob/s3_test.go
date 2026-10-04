package blob

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Presigning is pure computation (an HMAC over the request), so it can be
// tested with fake static credentials and no network access.
func TestS3PresignPut(t *testing.T) {
	client := s3.New(s3.Options{
		Region:      "ap-southeast-2",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "not-a-real-secret", ""),
	})
	store := NewS3(client, "tradedoc-test-bucket", "uploads/")

	before := time.Now()
	up, err := store.PresignPut(context.Background(), "doc_abc123", "application/pdf", 12345, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The exact origin matters: Terraform sets UPLOAD_ORIGIN (allowed by the
	// page's CSP connect-src) to https://<bucket>.s3.<region>.amazonaws.com.
	if origin := u.Scheme + "://" + u.Host; origin != "https://tradedoc-test-bucket.s3.ap-southeast-2.amazonaws.com" {
		t.Errorf("origin %q, want the bucket's regional virtual-hosted origin", origin)
	}
	if u.Path != "/uploads/doc_abc123" {
		t.Errorf("path %q, want /uploads/doc_abc123", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Signature") == "" {
		t.Error("X-Amz-Signature missing")
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Errorf("X-Amz-Expires = %q, want 300", q.Get("X-Amz-Expires"))
	}
	signed := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	for _, h := range []string{"host", "content-type", "content-length"} {
		if !contains(signed, h) {
			t.Errorf("%q is not a signed header (signed: %v)", h, signed)
		}
	}
	// A checksum header would have to be computed by the browser; we must
	// not ask for one.
	for _, h := range signed {
		if strings.Contains(h, "checksum") {
			t.Errorf("unexpected signed checksum header %q", h)
		}
	}

	if up.Method != "PUT" {
		t.Errorf("method %q, want PUT", up.Method)
	}
	if up.Headers["Content-Type"] != "application/pdf" {
		t.Errorf("headers %v: want Content-Type application/pdf", up.Headers)
	}
	for _, h := range []string{"Host", "Content-Length"} {
		if _, ok := up.Headers[h]; ok {
			t.Errorf("header %s must not be returned: browsers set it themselves", h)
		}
	}
	if up.ExpiresAt.Before(before.Add(5*time.Minute-time.Second)) || up.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
		t.Errorf("ExpiresAt %v is not ~5 minutes from now", up.ExpiresAt)
	}

	if _, err := store.PresignPut(context.Background(), "../secret", "application/pdf", 1, time.Minute); err == nil {
		t.Error("presigning a key with path characters should fail")
	}
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&types.NoSuchKey{}, true},
		{&types.NotFound{}, true},
		{&smithy.GenericAPIError{Code: "NotFound"}, true},
		{&smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{errors.New("network down"), false},
	}
	for _, tt := range tests {
		if got := isNotFound(tt.err); got != tt.want {
			t.Errorf("isNotFound(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
