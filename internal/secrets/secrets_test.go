package secrets

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// fakeHTTP answers every request with one canned response, so the real SDK
// client can be tested without network access.
type fakeHTTP struct {
	status int
	header http.Header
	body   string
}

func (f fakeHTTP) Do(r *http.Request) (*http.Response, error) {
	h := f.header
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", "application/x-amz-json-1.1")
	return &http.Response{
		StatusCode: f.status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Request:    r,
	}, nil
}

func client(f fakeHTTP) *secretsmanager.Client {
	return secretsmanager.New(secretsmanager.Options{
		Region:      "ap-southeast-2",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "not-a-real-secret", ""),
		HTTPClient:  f,
		Retryer:     aws.NopRetryer{}, // one attempt: the fake would give the same answer anyway
	})
}

const arn = "arn:aws:secretsmanager:ap-southeast-2:123456789012:secret:tradedoc/anthropic-api-key-AbCdEf"

func TestGet(t *testing.T) {
	tests := []struct {
		name    string
		resp    fakeHTTP
		want    string
		wantErr string
	}{
		{
			name: "value is trimmed",
			resp: fakeHTTP{status: 200, body: `{"ARN":"` + arn + `","SecretString":"  sk-test-value\n"}`},
			want: "sk-test-value",
		},
		{
			name:    "empty value",
			resp:    fakeHTTP{status: 200, body: `{"ARN":"` + arn + `","SecretString":" \n"}`},
			wantErr: "is empty",
		},
		{
			name:    "binary secret has no string value",
			resp:    fakeHTTP{status: 200, body: `{"ARN":"` + arn + `","SecretBinary":"AAEC"}`},
			wantErr: "is empty",
		},
		{
			// What Secrets Manager answers before `make set-secret` has run.
			name: "secret has no value yet",
			resp: fakeHTTP{
				status: 400,
				header: http.Header{"X-Amzn-Errortype": {"ResourceNotFoundException"}},
				body:   `{"__type":"ResourceNotFoundException","Message":"Secrets Manager can't find the specified secret value for staging label: AWSCURRENT"}`,
			},
			wantErr: "ResourceNotFoundException",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Get(context.Background(), client(tt.resp), arn)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Get = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
