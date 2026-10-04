package blob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3 stores files in an S3 bucket under a fixed prefix (e.g. "uploads/").
// The prefix keeps our objects in one place, so the IAM policy and the
// 30-day lifecycle rule can target exactly "uploads/*".
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	prefix  string
}

func NewS3(client *s3.Client, bucket, prefix string) *S3 {
	return &S3{client: client, presign: s3.NewPresignClient(client), bucket: bucket, prefix: prefix}
}

func (s *S3) objectKey(key string) *string { return aws.String(s.prefix + key) }

func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	if err := checkKey(key); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    s.objectKey(key),
		Body:   r,
	})
	return err
}

func (s *S3) Get(ctx context.Context, key string, max int64) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: s.objectKey(key)})
	if isNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	// S3 tells us the size up front, so an oversized object is refused
	// without downloading it.
	if out.ContentLength != nil && *out.ContentLength > max {
		return nil, ErrTooLarge
	}
	// Still cap the read itself (max+1, like Local) rather than trusting the header.
	data, err := io.ReadAll(io.LimitReader(out.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

func (s *S3) Size(ctx context.Context, key string) (int64, error) {
	if err := checkKey(key); err != nil {
		return 0, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: s.objectKey(key)})
	if isNotFound(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(out.ContentLength), nil
}

// Delete succeeds even if the object doesn't exist (S3 behaves that way).
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: s.objectKey(key)})
	return err
}

// PresignPut signs a PUT for one object with its content type and exact
// length. The browser then uploads straight to S3, so file bytes never pass
// through Lambda (which also caps request bodies at 6 MB).
//
// Content-Type and Content-Length are both part of the signature. We return
// the signed headers the browser must send, except Host and Content-Length:
// browsers set those two themselves and refuse to let scripts set them. The
// browser computes Content-Length from the file it sends, so a file of a
// different size than declared produces a different length, the signature
// no longer matches, and S3 rejects the upload with 403. That is what
// enforces the declared size on AWS.
func (s *S3) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (PresignedUpload, error) {
	if err := checkKey(key); err != nil {
		return PresignedUpload{}, err
	}
	// Taken before signing, so the expiry we report is never later than
	// the real one.
	expiresAt := time.Now().UTC().Add(ttl)
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           s.objectKey(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return PresignedUpload{}, err
	}

	headers := map[string]string{}
	for name, values := range req.SignedHeader {
		name = http.CanonicalHeaderKey(name) // "content-type" -> "Content-Type"
		if name == "Host" || name == "Content-Length" {
			continue
		}
		headers[name] = strings.Join(values, ",")
	}
	return PresignedUpload{URL: req.URL, Method: req.Method, Headers: headers, ExpiresAt: expiresAt}, nil
}

// isNotFound recognises S3's "no such object" errors. GetObject reports
// NoSuchKey; HeadObject has no response body, so it can only say NotFound.
// Note S3 answers 403 instead of 404 for a missing object if the caller may
// not list the bucket, which is why the Lambda role has s3:ListBucket.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var noKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound"
	}
	return false
}
