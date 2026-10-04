package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zeussilver/tradedoc/internal/domain"
	"github.com/zeussilver/tradedoc/internal/store"
	"github.com/zeussilver/tradedoc/internal/store/storetest"
)

// Most of these tests need a real DynamoDB API, so they only run against
// DynamoDB Local (a free Docker image) when DYNAMODB_ENDPOINT is set:
//
//	docker run -d --rm -p 8000:8000 --name tradedoc-ddb amazon/dynamodb-local
//	DYNAMODB_ENDPOINT=http://localhost:8000 go test -race ./internal/store/...
//	docker stop tradedoc-ddb
//
// Without it they are skipped, so `go test ./...` never touches the network.
// TestDynamoReviewCancellationReasons always runs: it fakes DynamoDB.

func TestDynamo(t *testing.T) {
	dynamoEndpoint(t) // skip once here rather than in each subtest
	storetest.Run(t, newDynamo)
}

// Memory overwrites duplicates; Dynamo refuses them, so these checks live
// here rather than in the shared suite. Duplicate documents and suggestions
// go through TransactWriteItems, which reports a failed condition
// differently from a single write.
func TestDynamoRejectsDuplicates(t *testing.T) {
	db := newDynamo(t)
	ctx := t.Context()

	doc := domain.Document{ID: "doc_1", ShipmentID: "shp_1", Status: domain.StatusPendingUpload}
	if err := db.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateDocument(ctx, doc); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second CreateDocument: got %v, want ErrConflict", err)
	}

	sug := []domain.ClassificationSuggestion{{ID: "sug_1", ShipmentID: "shp_1", Status: domain.SuggestionPending}}
	if err := db.AddSuggestions(ctx, sug); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSuggestions(ctx, sug); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second AddSuggestions: got %v, want ErrConflict", err)
	}
}

// An extraction is filed under its document's shipment, so the document
// has to exist first.
func TestDynamoExtractionNeedsDocument(t *testing.T) {
	db := newDynamo(t)
	err := db.AddExtraction(t.Context(), domain.Extraction{ID: "ext_1", DocumentID: "doc_missing"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// DynamoDB items are limited to 400 KB. A write over that must come back as
// store.ErrTooLarge (which the API answers with 413), not as an unknown
// error that turns into a 500. Memory has no such limit, so this test is
// Dynamo-only. Each kind of write is covered because DynamoDB reports the
// limit from PutItem, from a transaction's Put and from a transaction's
// Update.
func TestDynamoTooLarge(t *testing.T) {
	db := newDynamo(t)
	ctx := t.Context()
	big := strings.Repeat("x", 410*1024)

	// A suggestion just under the limit, so that adding a long review note
	// to it pushes the stored item over.
	nearLimit := domain.ClassificationSuggestion{ID: "sug_near", ShipmentID: "shp_1", ExtractionID: "ext_1", LineNo: 1,
		Rationale: strings.Repeat("x", 395*1024), Status: domain.SuggestionPending}
	if err := db.AddSuggestions(ctx, []domain.ClassificationSuggestion{nearLimit}); err != nil {
		t.Fatal(err)
	}
	existing := domain.Document{ID: "doc_1", ShipmentID: "shp_1", Status: domain.StatusPendingUpload}
	if err := db.CreateDocument(ctx, existing); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		write func() error
	}{
		{"PutItem", func() error {
			return db.AddCheckRun(ctx, domain.CheckRun{ID: "chk_1", ShipmentID: "shp_1", Findings: []domain.Finding{{Message: big}}})
		}},
		{"PutItem replacing an item", func() error {
			d := existing
			d.Filename = big
			return db.UpdateDocument(ctx, d)
		}},
		{"transaction Put", func() error {
			return db.CreateDocument(ctx, domain.Document{ID: "doc_2", ShipmentID: "shp_1", Filename: big})
		}},
		{"transaction Put, suggestions", func() error {
			return db.AddSuggestions(ctx, []domain.ClassificationSuggestion{{ID: "sug_big", ShipmentID: "shp_1", Rationale: big}})
		}},
		{"transaction Update", func() error {
			r := domain.Review{Status: domain.SuggestionAccepted, Reviewer: "Alice", Note: strings.Repeat("y", 10*1024)}
			_, err := db.ReviewSuggestion(ctx, "sug_near", r)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.write(); !errors.Is(err, store.ErrTooLarge) {
				t.Fatalf("got %v, want ErrTooLarge", err)
			}
		})
	}

	// The refused review was a transaction, so the suggestion is untouched
	// and the line got no "accepted" marker: a shorter note still works.
	r := domain.Review{Status: domain.SuggestionAccepted, Reviewer: "Alice", Note: "short"}
	if _, err := db.ReviewSuggestion(ctx, "sug_near", r); err != nil {
		t.Fatalf("review with a short note after the refused one: %v", err)
	}
}

// DynamoDB Local runs transactions one at a time, so it never reports the
// "TransactionConflict" cancellation that real DynamoDB gives when two
// transactions write the same item at once, nor every way real DynamoDB
// words a failed step. This test plays DynamoDB with canned answers from an
// in-process HTTP server (no network, no Docker) to check how
// ReviewSuggestion reads each cancelled transaction. Accepting is two steps:
// the status update (reason 0) and the line marker (reason 1).
func TestDynamoReviewCancellationReasons(t *testing.T) {
	const (
		ok            = ""
		alreadyDone   = `[{"Code":"ConditionalCheckFailed"},{"Code":"ConditionalCheckFailed"}]`
		lineTaken     = `[{"Code":"None"},{"Code":"ConditionalCheckFailed"}]`
		clashStatus   = `[{"Code":"TransactionConflict"},{"Code":"None"}]`
		clashBoth     = `[{"Code":"TransactionConflict"},{"Code":"ConditionalCheckFailed"}]`
		updateTooLong = `[{"Code":"ValidationError","Message":"Item size to update has exceeded the maximum allowed size"},{"Code":"None"}]`
	)
	accept := domain.Review{Status: domain.SuggestionAccepted, Reviewer: "Alice"}
	tests := []struct {
		name     string
		answers  []string // one per transaction attempt
		want     error
		attempts int64
	}{
		{"accepted", []string{ok}, nil, 1},
		{"already decided", []string{alreadyDone}, store.ErrConflict, 1},
		{"line already has a code", []string{lineTaken}, store.ErrLineAccepted, 1},
		{"clash, then accepted", []string{clashStatus, ok}, nil, 2},
		{"clash, then line already has a code", []string{clashStatus, lineTaken}, store.ErrLineAccepted, 2},
		// A failed marker check beside a clash may be another reviewer
		// accepting this same suggestion (ErrConflict, not
		// ErrLineAccepted), so that is tried again too.
		{"clash beside a failed check", []string{clashBoth, ok}, nil, 2},
		{"item would be too large", []string{updateTooLong}, store.ErrTooLarge, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, attempts := fakeDynamo(t, tc.answers)
			_, err := db.ReviewSuggestion(t.Context(), "sug_1", accept)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if got := attempts.Load(); got != tc.attempts {
				t.Fatalf("%d transaction attempts, want %d", got, tc.attempts)
			}
		})
	}

	// It must not retry forever: after a few clashes it gives up and
	// returns DynamoDB's error.
	db, attempts := fakeDynamo(t, []string{clashStatus, clashStatus, clashStatus, clashStatus, clashStatus, ok})
	_, err := db.ReviewSuggestion(t.Context(), "sug_1", accept)
	var cancelled *types.TransactionCanceledException
	if !errors.As(err, &cancelled) {
		t.Fatalf("got %v, want DynamoDB's TransactionCanceledException", err)
	}
	if got := attempts.Load(); got != 5 {
		t.Fatalf("%d transaction attempts, want 5", got)
	}
}

// fakeDynamo returns a Dynamo store talking to a fake DynamoDB that holds one
// pending suggestion, sug_1, and answers the n-th TransactWriteItems call
// with answers[n-1]: "" for success, or the JSON list of cancellation
// reasons to cancel it with. It also returns the number of those calls.
func fakeDynamo(t *testing.T, answers []string) (*store.Dynamo, *atomic.Int64) {
	t.Helper()
	sug, err := json.Marshal(domain.ClassificationSuggestion{
		ID: "sug_1", ShipmentID: "shp_1", ExtractionID: "ext_1", LineNo: 1, Rank: 2, Status: domain.SuggestionPending})
	if err != nil {
		t.Fatal(err)
	}
	// ReviewSuggestion reads the lookup item and then the suggestion's own
	// item. One item with the attributes of both can answer either read.
	item, err := json.Marshal(map[string]any{"Item": map[string]any{
		"pk":          map[string]string{"S": "SHP#shp_1"},
		"sk":          map[string]string{"S": "SUG#1"},
		"shipment_id": map[string]string{"S": "shp_1"},
		"item_sk":     map[string]string{"S": "SUG#1"},
		"status":      map[string]string{"S": "pending"},
		"data":        map[string]string{"S": string(sug)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		// The DynamoDB API is one URL; the operation is named in a header.
		switch r.Header.Get("X-Amz-Target") {
		case "DynamoDB_20120810.GetItem":
			_, _ = w.Write(item)
		case "DynamoDB_20120810.TransactWriteItems":
			n := calls.Add(1)
			if n > int64(len(answers)) {
				http.Error(w, "more calls than the test expected", http.StatusNotImplemented)
				return
			}
			answer := answers[n-1]
			if answer == "" {
				_, _ = w.Write([]byte("{}"))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"__type":"com.amazonaws.dynamodb.v20120810#TransactionCanceledException",`+
				`"Message":"Transaction cancelled","CancellationReasons":%s}`, answer)
		default:
			http.Error(w, "the fake doesn't support this call", http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)

	client := dynamodb.New(dynamodb.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("fake", "fake", ""),
		RetryMaxAttempts: 1, // count only the store's own attempts, not the SDK's
	})
	return store.NewDynamo(client, "tradedoc-fake"), &calls
}

// dynamoEndpoint returns DYNAMODB_ENDPOINT, or skips the test if it isn't set.
func dynamoEndpoint(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("set DYNAMODB_ENDPOINT=http://localhost:8000 and run DynamoDB Local to test the DynamoDB store (see dynamo_test.go)")
	}
	return endpoint
}

var tableSeq atomic.Int64

// newDynamo returns a Dynamo store backed by a new, empty table shaped like
// the real one (string keys pk and sk), deleted again after the test.
func newDynamo(t *testing.T) store.Store {
	t.Helper()
	endpoint := dynamoEndpoint(t)
	client := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		// DynamoDB Local accepts any credentials. Fixed fake ones mean the
		// test never picks up the developer's real AWS profile.
		Credentials: credentials.NewStaticCredentialsProvider("fake", "fake", ""),
	})

	ctx := t.Context()
	table := fmt.Sprintf("tradedoc-test-%d-%d", time.Now().UnixNano(), tableSeq.Add(1))
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("create table (is DynamoDB Local running at %s?): %v", endpoint, err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		_, _ = client.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	})
	waiter := dynamodb.NewTableExistsWaiter(client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)}, time.Minute); err != nil {
		t.Fatalf("wait for table: %v", err)
	}
	return store.NewDynamo(client, table)
}
