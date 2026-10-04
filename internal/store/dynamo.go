package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/zeussilver/tradedoc/internal/domain"
)

// Dynamo is the Store used on AWS. Everything lives in one DynamoDB table
// (a "single-table design"): the table has a string partition key pk and a
// string sort key sk, and each kind of record gets its own key pattern.
//
// Every item has:
//
//	pk, sk      the key (patterns below)
//	data        the domain record as JSON. Storing JSON instead of one
//	            attribute per field keeps the generic Field[T] types simple
//	            and keeps the item readable in the AWS console.
//	expires_at  Unix seconds, now + 30 days. The table's TTL setting deletes
//	            items after that, so demo data cleans itself up.
//
// All records that belong to a shipment share its partition, so "everything
// for shipment X of kind Y" is one Query on pk with a sort-key prefix:
//
//	Shipment      pk=SHP#<sid>  sk=SHP
//	Document      pk=SHP#<sid>  sk=DOC#<docID>
//	Extraction    pk=SHP#<sid>  sk=EXT#<docID>#<ts>#<id>
//	CheckRun      pk=SHP#<sid>  sk=CHK#<ts>#<id>
//	Suggestion    pk=SHP#<sid>  sk=SUG#<ts>#<pos>#<id>
//	SignOff       pk=SHP#<sid>  sk=SGN#<ts>#<id>
//	AuditEvent    pk=SHP#<sid>  sk=AUD#<ts>#<id>
//
// Some calls only have a document or suggestion ID, not its shipment, so
// those records also get a small lookup item pointing at their shipment:
//
//	pk=DOC#<docID>  sk=REF  shipment_id
//	pk=SUG#<id>     sk=REF  shipment_id, item_sk
//
// An invoice line can have only one accepted candidate code, so accepting
// one also creates a marker item named after the line (see ReviewSuggestion):
//
//	pk=SHP#<sid>  sk=ACC#<extractionID>#<lineNo>  suggestion_id
//
// The daily model-call counter is its own item: pk=QUOTA#<key>, sk=QUOTA.
//
// DynamoDB sorts items by sk as plain strings, and <ts> is written so that
// string order is time order. That is how lists come back oldest first.
type Dynamo struct {
	client *dynamodb.Client
	table  string
}

var _ Store = (*Dynamo)(nil) // compile-time check that Dynamo implements Store

func NewDynamo(client *dynamodb.Client, table string) *Dynamo {
	return &Dynamo{client: client, table: table}
}

const (
	itemTTL = 30 * 24 * time.Hour
	// quotaTTL is much shorter: a daily counter is useless after its day, and
	// per-IP counters hold a client IP address, which we shouldn't keep longer
	// than we need it.
	quotaTTL = 48 * time.Hour

	// tsLayout is fixed width (always 9 fraction digits, always UTC), so
	// comparing two timestamps as strings gives the same answer as
	// comparing them as times. RFC3339Nano would be wrong here: it drops
	// trailing zeros, writing "09:00:00Z" but "09:00:00.25Z", and since
	// "." sorts before "Z" the later time would sort first.
	tsLayout = "2006-01-02T15:04:05.000000000Z"

	// maxTransactItems is DynamoDB's limit on actions in one transaction.
	maxTransactItems = 100

	// maxReviewAttempts is how many times ReviewSuggestion tries its
	// transaction when DynamoDB cancels it for clashing with another one.
	maxReviewAttempts = 5
)

// ---- shipments ----

func (db *Dynamo) CreateShipment(ctx context.Context, s domain.Shipment) error {
	it, err := newItem(shipmentPK(s.ID), "SHP", s)
	if err != nil {
		return err
	}
	return db.add(ctx, it)
}

func (db *Dynamo) GetShipment(ctx context.Context, id string) (domain.Shipment, error) {
	var s domain.Shipment
	it, err := db.get(ctx, shipmentPK(id), "SHP")
	if err != nil {
		return s, err
	}
	err = decode(it, &s)
	return s, err
}

// ---- documents ----

// CreateDocument writes the document and its lookup item in one
// transaction, so there is never a document that GetDocument can't find.
func (db *Dynamo) CreateDocument(ctx context.Context, d domain.Document) error {
	it, err := documentItem(d)
	if err != nil {
		return err
	}
	ref := map[string]types.AttributeValue{
		"pk":          str("DOC#" + d.ID),
		"sk":          str("REF"),
		"shipment_id": str(d.ShipmentID),
		"expires_at":  expiresAt(),
	}
	_, err = db.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{db.putNew(it), db.putNew(ref)},
	})
	return writeErr(err, ErrConflict)
}

func (db *Dynamo) GetDocument(ctx context.Context, id string) (domain.Document, error) {
	shipmentID, err := db.documentShipment(ctx, id)
	if err != nil {
		return domain.Document{}, err
	}
	it, err := db.get(ctx, shipmentPK(shipmentID), "DOC#"+id)
	if err != nil {
		return domain.Document{}, err
	}
	return decodeDocument(it)
}

func (db *Dynamo) ListDocuments(ctx context.Context, shipmentID string) ([]domain.Document, error) {
	items, err := db.query(ctx, shipmentPK(shipmentID), "DOC#")
	if err != nil {
		return nil, err
	}
	out := make([]domain.Document, 0, len(items))
	for _, it := range items {
		d, err := decodeDocument(it)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	// Document keys hold no time (UpdateDocument has to find the item from
	// the ID alone), so DynamoDB returns them in ID order; sort by time here.
	sort.SliceStable(out, func(i, j int) bool { return out[i].UploadedAt.Before(out[j].UploadedAt) })
	return out, nil
}

// UpdateDocument overwrites the document item. The condition makes it fail
// instead of creating a new item when the document doesn't exist.
func (db *Dynamo) UpdateDocument(ctx context.Context, d domain.Document) error {
	it, err := documentItem(d)
	if err != nil {
		return err
	}
	_, err = db.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(db.table),
		Item:                it,
		ConditionExpression: aws.String("attribute_exists(pk)"),
	})
	return writeErr(err, ErrNotFound)
}

// documentItem stores StorageKey as its own attribute: the field is tagged
// json:"-" so it never reaches API clients, which also keeps it out of data.
func documentItem(d domain.Document) (map[string]types.AttributeValue, error) {
	it, err := newItem(shipmentPK(d.ShipmentID), "DOC#"+d.ID, d)
	if err != nil {
		return nil, err
	}
	it["storage_key"] = str(d.StorageKey)
	return it, nil
}

func decodeDocument(it map[string]types.AttributeValue) (domain.Document, error) {
	var d domain.Document
	if err := decode(it, &d); err != nil {
		return d, err
	}
	d.StorageKey = stringAttr(it, "storage_key")
	return d, nil
}

// documentShipment reads a document's lookup item to find its shipment.
func (db *Dynamo) documentShipment(ctx context.Context, docID string) (string, error) {
	ref, err := db.get(ctx, "DOC#"+docID, "REF")
	if err != nil {
		return "", err
	}
	return stringAttr(ref, "shipment_id"), nil
}

// ---- extractions ----

// AddExtraction returns ErrNotFound if the document doesn't exist, because
// the extraction has to be filed under the document's shipment.
func (db *Dynamo) AddExtraction(ctx context.Context, e domain.Extraction) error {
	shipmentID, err := db.documentShipment(ctx, e.DocumentID)
	if err != nil {
		return err
	}
	// The trailing "#" after the document ID matters: without it, listing
	// "doc_1" would also match the extractions of "doc_10".
	sk := "EXT#" + e.DocumentID + "#" + timestamp(e.CreatedAt) + "#" + e.ID
	it, err := newItem(shipmentPK(shipmentID), sk, e)
	if err != nil {
		return err
	}
	return db.add(ctx, it)
}

func (db *Dynamo) ListExtractions(ctx context.Context, documentID string) ([]domain.Extraction, error) {
	shipmentID, err := db.documentShipment(ctx, documentID)
	if errors.Is(err, ErrNotFound) {
		return []domain.Extraction{}, nil // same as Memory: an unknown document has no extractions
	}
	if err != nil {
		return nil, err
	}
	return list[domain.Extraction](ctx, db, shipmentPK(shipmentID), "EXT#"+documentID+"#")
}

// ---- check runs ----

func (db *Dynamo) AddCheckRun(ctx context.Context, r domain.CheckRun) error {
	it, err := newItem(shipmentPK(r.ShipmentID), "CHK#"+timestamp(r.CreatedAt)+"#"+r.ID, r)
	if err != nil {
		return err
	}
	return db.add(ctx, it)
}

func (db *Dynamo) ListCheckRuns(ctx context.Context, shipmentID string) ([]domain.CheckRun, error) {
	return list[domain.CheckRun](ctx, db, shipmentPK(shipmentID), "CHK#")
}

// ---- classification suggestions ----

// AddSuggestions writes each suggestion plus its lookup item. The handler
// gives every candidate from one model call the same CreatedAt, so the key
// also holds the suggestion's position in this call (<pos>): that keeps
// them in the order they were added instead of random ID order.
//
// A transaction holds at most 100 actions, i.e. 50 suggestions, so larger
// batches are written in several transactions. Each suggestion's two items
// are always in the same transaction (100 is even), so a suggestion never
// exists without its lookup item.
func (db *Dynamo) AddSuggestions(ctx context.Context, sugs []domain.ClassificationSuggestion) error {
	var steps []types.TransactWriteItem
	for i, s := range sugs {
		sk := fmt.Sprintf("SUG#%s#%06d#%s", timestamp(s.CreatedAt), i, s.ID)
		it, err := newItem(shipmentPK(s.ShipmentID), sk, s)
		if err != nil {
			return err
		}
		// status is copied out of the JSON into its own attribute so that
		// ReviewSuggestion can put a condition on it.
		it["status"] = str(string(s.Status))
		ref := map[string]types.AttributeValue{
			"pk":          str("SUG#" + s.ID),
			"sk":          str("REF"),
			"shipment_id": str(s.ShipmentID),
			"item_sk":     str(sk),
			"expires_at":  expiresAt(),
		}
		steps = append(steps, db.putNew(it), db.putNew(ref))
	}
	for start := 0; start < len(steps); start += maxTransactItems {
		end := min(start+maxTransactItems, len(steps))
		_, err := db.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
			TransactItems: steps[start:end],
		})
		if err != nil {
			return writeErr(err, ErrConflict)
		}
	}
	return nil
}

func (db *Dynamo) ListSuggestions(ctx context.Context, shipmentID string) ([]domain.ClassificationSuggestion, error) {
	return list[domain.ClassificationSuggestion](ctx, db, shipmentPK(shipmentID), "SUG#")
}

func (db *Dynamo) GetSuggestion(ctx context.Context, id string) (domain.ClassificationSuggestion, error) {
	var s domain.ClassificationSuggestion
	it, err := db.suggestionItem(ctx, id)
	if err != nil {
		return s, err
	}
	err = decode(it, &s)
	return s, err
}

// ReviewSuggestion reads the suggestion, applies the decision, and writes it
// back in one transaction. Its steps either all happen or none do:
//
//  1. update the suggestion, only if its stored status is still pending;
//  2. when accepting, also create the line's marker item (see the key list
//     on Dynamo), only if the line doesn't have one yet.
//
// Two reviewers can both read "pending", but DynamoDB checks the conditions
// and applies the writes as one step, so only the first decision is stored:
// a second decision on the same suggestion gets ErrConflict instead of
// silently replacing the first, and accepting a second candidate for a line
// that already has an accepted one gets ErrLineAccepted. Rejecting doesn't
// touch the marker, so the other candidates of a line can still be rejected.
// Decisions are final, so a marker is never removed.
func (db *Dynamo) ReviewSuggestion(ctx context.Context, id string, r domain.Review) (domain.ClassificationSuggestion, error) {
	for attempt := 1; ; attempt++ {
		s, err := db.reviewOnce(ctx, id, r)
		// When two transactions write the same item at the same moment (two
		// reviewers clicking together), DynamoDB may cancel one of them with
		// "TransactionConflict" rather than decide it. Trying again reads
		// the new state and gives the real answer.
		if !isTransactionConflict(err) || attempt == maxReviewAttempts {
			return s, err
		}
		// Wait a short random time first, so the two don't clash again.
		select {
		case <-ctx.Done():
			return domain.ClassificationSuggestion{}, ctx.Err()
		case <-time.After(rand.N(time.Duration(attempt) * 20 * time.Millisecond)):
		}
	}
}

// reviewOnce is one attempt of ReviewSuggestion.
func (db *Dynamo) reviewOnce(ctx context.Context, id string, r domain.Review) (domain.ClassificationSuggestion, error) {
	var s domain.ClassificationSuggestion
	it, err := db.suggestionItem(ctx, id)
	if err != nil {
		return s, err
	}
	if err := decode(it, &s); err != nil {
		return s, err
	}
	if s.Status != domain.SuggestionPending {
		return s, ErrConflict
	}
	at := r.At
	s.Status, s.ReviewedBy, s.ReviewNote, s.ReviewedAt = r.Status, r.Reviewer, r.Note, &at
	data, err := json.Marshal(s)
	if err != nil {
		return domain.ClassificationSuggestion{}, err
	}
	steps := []types.TransactWriteItem{{Update: &types.Update{
		TableName:           aws.String(db.table),
		Key:                 map[string]types.AttributeValue{"pk": it["pk"], "sk": it["sk"]},
		UpdateExpression:    aws.String("SET #data = :data, #status = :decision"),
		ConditionExpression: aws.String("#status = :pending"),
		// "data" and "status" are DynamoDB reserved words, so expressions
		// must refer to them through #placeholders.
		ExpressionAttributeNames: map[string]string{"#data": "data", "#status": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":data":     str(string(data)),
			":decision": str(string(r.Status)),
			":pending":  str(string(domain.SuggestionPending)),
		},
	}}}
	if r.Status == domain.SuggestionAccepted {
		// The marker's key is the line itself, so a line can only ever have
		// one. suggestion_id records which candidate holds it.
		marker := map[string]types.AttributeValue{
			"pk":            str(shipmentPK(s.ShipmentID)),
			"sk":            str(fmt.Sprintf("ACC#%s#%d", s.ExtractionID, s.LineNo)),
			"suggestion_id": str(s.ID),
			"expires_at":    expiresAt(),
		}
		steps = append(steps, db.putNew(marker))
	}
	_, err = db.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: steps})

	// A cancelled transaction lists one reason per step, in the order the
	// steps were sent: step 0 is the status check, step 1 the marker. The
	// status is looked at first, as Memory does: a suggestion that was
	// already decided is a conflict whatever state its line is in.
	//
	// A clash is looked at before both. A failed marker check beside a
	// clash may come from another reviewer accepting this same suggestion,
	// which must be reported as ErrConflict, not ErrLineAccepted; only a
	// fresh attempt can tell.
	switch {
	case err == nil:
		return s, nil
	case isTransactionConflict(err):
		return domain.ClassificationSuggestion{}, err // ReviewSuggestion tries again
	case cancelReason(err, 0) == "ConditionalCheckFailed":
		return domain.ClassificationSuggestion{}, ErrConflict // another reviewer got there first
	case cancelReason(err, 1) == "ConditionalCheckFailed":
		return domain.ClassificationSuggestion{}, ErrLineAccepted // another candidate for this line was accepted first
	case isTooLarge(err):
		return domain.ClassificationSuggestion{}, ErrTooLarge
	}
	return domain.ClassificationSuggestion{}, err
}

// suggestionItem follows the lookup item to the suggestion's own item.
func (db *Dynamo) suggestionItem(ctx context.Context, id string) (map[string]types.AttributeValue, error) {
	ref, err := db.get(ctx, "SUG#"+id, "REF")
	if err != nil {
		return nil, err
	}
	return db.get(ctx, shipmentPK(stringAttr(ref, "shipment_id")), stringAttr(ref, "item_sk"))
}

// ---- sign-offs and audit ----

func (db *Dynamo) AddSignOff(ctx context.Context, s domain.SignOff) error {
	it, err := newItem(shipmentPK(s.ShipmentID), "SGN#"+timestamp(s.CreatedAt)+"#"+s.ID, s)
	if err != nil {
		return err
	}
	return db.add(ctx, it)
}

func (db *Dynamo) ListSignOffs(ctx context.Context, shipmentID string) ([]domain.SignOff, error) {
	return list[domain.SignOff](ctx, db, shipmentPK(shipmentID), "SGN#")
}

func (db *Dynamo) AppendAudit(ctx context.Context, e domain.AuditEvent) error {
	it, err := newItem(shipmentPK(e.ShipmentID), "AUD#"+timestamp(e.At)+"#"+e.ID, e)
	if err != nil {
		return err
	}
	return db.add(ctx, it)
}

func (db *Dynamo) ListAudit(ctx context.Context, shipmentID string) ([]domain.AuditEvent, error) {
	return list[domain.AuditEvent](ctx, db, shipmentPK(shipmentID), "AUD#")
}

// ---- quota ----

// TakeQuota adds 1 to the counter only while it is below limit. DynamoDB
// checks the condition and does the ADD as one atomic step, so two Lambda
// instances racing at limit-1 can't both get through.
func (db *Dynamo) TakeQuota(ctx context.Context, key string, limit int64) (bool, error) {
	// A brand-new counter passes "attribute_not_exists(n)", which would let
	// one call through even with a limit of 0, so that case is decided here.
	if limit <= 0 {
		return false, nil
	}
	_, err := db.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(db.table),
		Key:                 map[string]types.AttributeValue{"pk": str("QUOTA#" + key), "sk": str("QUOTA")},
		UpdateExpression:    aws.String("ADD n :one SET expires_at = :expires"),
		ConditionExpression: aws.String("attribute_not_exists(n) OR n < :limit"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one":     num(1),
			":limit":   num(limit),
			":expires": num(time.Now().Add(quotaTTL).Unix()),
		},
	})
	if isConditionFailed(err) {
		return false, nil // limit reached: not an error, just "no"
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ---- helpers ----

func shipmentPK(id string) string { return "SHP#" + id }

// timestamp formats t for use in a sort key (see tsLayout).
func timestamp(t time.Time) string { return t.UTC().Format(tsLayout) }

func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

// DynamoDB sends numbers as strings on the wire, to keep their exact value.
func num(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func expiresAt() types.AttributeValue { return num(time.Now().Add(itemTTL).Unix()) }

// stringAttr returns a string attribute, or "" if it is missing.
func stringAttr(it map[string]types.AttributeValue, name string) string {
	if v, ok := it[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// newItem builds a table item holding v as JSON in its data attribute.
func newItem(pk, sk string, v any) (map[string]types.AttributeValue, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return map[string]types.AttributeValue{
		"pk":         str(pk),
		"sk":         str(sk),
		"data":       str(string(data)),
		"expires_at": expiresAt(),
	}, nil
}

// decode reads the JSON in an item's data attribute into v.
func decode(it map[string]types.AttributeValue, v any) error {
	return json.Unmarshal([]byte(stringAttr(it, "data")), v)
}

// add writes a new item and refuses to overwrite an existing one. These
// records are append-only, so a repeated key means a bug, not an update.
func (db *Dynamo) add(ctx context.Context, it map[string]types.AttributeValue) error {
	_, err := db.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(db.table),
		Item:                it,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	return writeErr(err, ErrConflict)
}

// putNew is the transaction version of add.
func (db *Dynamo) putNew(it map[string]types.AttributeValue) types.TransactWriteItem {
	return types.TransactWriteItem{Put: &types.Put{
		TableName:           aws.String(db.table),
		Item:                it,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	}}
}

// get reads one item, or returns ErrNotFound. ConsistentRead makes sure we
// see writes that just finished (e.g. a document created by the previous
// request); the default read may briefly return older data.
func (db *Dynamo) get(ctx context.Context, pk, sk string) (map[string]types.AttributeValue, error) {
	res, err := db.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(db.table),
		Key:            map[string]types.AttributeValue{"pk": str(pk), "sk": str(sk)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if len(res.Item) == 0 {
		return nil, ErrNotFound
	}
	return res.Item, nil
}

// query returns every item in partition pk whose sort key starts with
// prefix, in sort-key order. DynamoDB returns at most 1 MB per call and
// sets LastEvaluatedKey when there is more, so we keep asking from there.
func (db *Dynamo) query(ctx context.Context, pk, prefix string) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	var startKey map[string]types.AttributeValue
	for {
		res, err := db.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(db.table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :prefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":     str(pk),
				":prefix": str(prefix),
			},
			ConsistentRead:    aws.Bool(true),
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, err
		}
		items = append(items, res.Items...)
		if len(res.LastEvaluatedKey) == 0 {
			return items, nil
		}
		startKey = res.LastEvaluatedKey
	}
}

// list queries a prefix and decodes each item's JSON. Go methods can't have
// type parameters, so this is a function that takes the store.
func list[T any](ctx context.Context, db *Dynamo, pk, prefix string) ([]T, error) {
	items, err := db.query(ctx, pk, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(items)) // never nil: a nil slice encodes as JSON null, not []
	for _, it := range items {
		var v T
		if err := decode(it, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// writeErr turns the write failures a caller can act on into store errors:
// a false condition becomes onCondition (what that means depends on the
// write), and an item over DynamoDB's size limit becomes ErrTooLarge, so
// the API can answer 413 instead of 500. Anything else, including nil, is
// returned unchanged.
func writeErr(err, onCondition error) error {
	switch {
	case isConditionFailed(err):
		return onCondition
	case isTooLarge(err):
		return ErrTooLarge
	}
	return err
}

// isConditionFailed reports whether DynamoDB refused a write because its
// condition was false. A single write fails with
// ConditionalCheckFailedException; a transaction fails with
// TransactionCanceledException, which lists a reason for each step.
func isConditionFailed(err error) bool {
	var single *types.ConditionalCheckFailedException
	return errors.As(err, &single) || hasCancelReason(err, "ConditionalCheckFailed")
}

// isTransactionConflict reports whether DynamoDB cancelled a transaction
// because another request was writing one of the same items at that moment.
func isTransactionConflict(err error) bool {
	return hasCancelReason(err, "TransactionConflict")
}

// isTooLarge reports whether DynamoDB refused a write because an item would
// be over its 400 KB limit. There is no error type for that: it is a
// ValidationException whose message says "Item size has exceeded the
// maximum allowed size" ("Item size to update has exceeded..." for an
// update). Inside a transaction DynamoDB may instead give it as the
// cancellation reason of one step, with code ValidationError.
func isTooLarge(err error) bool {
	sizeMessage := func(msg string) bool {
		return strings.Contains(msg, "Item size") && strings.Contains(msg, "exceeded the maximum allowed size")
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException" && sizeMessage(apiErr.ErrorMessage()) {
		return true
	}
	var tx *types.TransactionCanceledException
	if errors.As(err, &tx) {
		for _, reason := range tx.CancellationReasons {
			if aws.ToString(reason.Code) == "ValidationError" && sizeMessage(aws.ToString(reason.Message)) {
				return true
			}
		}
	}
	return false
}

// hasCancelReason reports whether err is a cancelled transaction in which
// any step failed with the given reason code.
func hasCancelReason(err error, code string) bool {
	var tx *types.TransactionCanceledException
	if errors.As(err, &tx) {
		for _, reason := range tx.CancellationReasons {
			if aws.ToString(reason.Code) == code {
				return true
			}
		}
	}
	return false
}

// cancelReason returns the reason code DynamoDB gave for step i of a
// cancelled transaction ("None" if that step was fine), or "" if err is not
// a cancelled transaction. Reasons come back in the order the steps were sent.
func cancelReason(err error, i int) string {
	var tx *types.TransactionCanceledException
	if errors.As(err, &tx) && i < len(tx.CancellationReasons) {
		return aws.ToString(tx.CancellationReasons[i].Code)
	}
	return ""
}
