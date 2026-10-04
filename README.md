# TradeDoc Check

An AI-assisted checker for trade documents (commercial invoice + packing list).
The goal: extract fields with Claude, validate them against a strict schema,
flag inconsistencies between documents, and suggest candidate tariff
classifications **for a customs broker to review**. Every step is written to an
audit trail.

> Status: day 1–2. The Go API and data schema are in place. Claude extraction,
> cross-document checks, the React/Vite frontend and AWS deployment come next.

## Run

```bash
go test -race ./...          # unit + end-to-end API tests
go run ./cmd/api             # listens on :8080 (ADDR, DATA_DIR env vars)
```

```bash
B=http://localhost:8080
curl -s -X POST $B/v1/shipments -d '{"reference":"JOB-001"}'
curl -s -X POST $B/v1/shipments/<shipment_id>/documents -F type=commercial_invoice -F file=@invoice.pdf
curl -s -X POST $B/v1/documents/<document_id>/extractions -d @testdata/invoice_extraction.json
curl -s $B/v1/shipments/<shipment_id>/audit
```

## API

| Method | Path | Purpose |
|---|---|---|
| GET  | `/healthz` | liveness check |
| POST | `/v1/shipments` | create a shipment (groups the documents for one consignment) |
| GET  | `/v1/shipments/{id}` | get a shipment |
| POST | `/v1/shipments/{id}/documents` | upload a PDF/PNG/JPEG (multipart: `type`, `file`) |
| GET  | `/v1/shipments/{id}/documents` | list documents |
| POST | `/v1/documents/{id}/extractions` | submit an extraction; validated against the schema for the document type |
| GET  | `/v1/documents/{id}/extractions` | list all extractions (append-only history) |
| GET  | `/v1/shipments/{id}/audit` | audit trail |

Errors always look like `{"error": {"code", "message", "details"}}`.
Validation failures return **422** with every problem as `{path, message}`.

## Layout

```
cmd/api/            entry point: config, logging, graceful shutdown
internal/domain/    data model and validation rules (no HTTP, no storage)
internal/httpapi/   routes, handlers, middleware, JSON helpers
internal/store/     Store interface + in-memory implementation
internal/blob/      file storage interface + local disk (S3 later)
testdata/           sample extraction payloads
```

## Schema decisions

- **Every extracted value carries evidence.** `Field[T]` holds the value, a
  confidence (`high`/`medium`/`low`) and the page + exact quote it came from.
  A value without a quote is rejected.
- **`null` means "not in the document".** Values are pointers, so a missing
  field is distinguishable from an empty string or zero; the model must not guess.
- **Money and weight are integers** (cents, grams). No floats in totals.
- **Codes are validated as codes**: ISO 4217 currency, ISO 3166 country,
  Incoterms 2020, HS codes as 6- or 8-digit strings (leading zeros matter).
- **Schema validation ≠ business checks.** Validation asks "is this shaped
  correctly?". Whether line totals add up, or the packing list matches the
  invoice, is a *finding* about a real document and is reported, not rejected.
- **Extractions are append-only**, so model output and human corrections are
  both kept.
- **Classification output is a suggestion** with `pending/accepted/rejected`
  status and a reviewer; the system never finalises a classification.

## Security basics

- Upload size capped at 10 MiB; type decided by sniffing file bytes, not the
  extension or client header; files stored under server-generated keys.
- JSON bodies capped at 1 MiB; unknown fields rejected.
- Logs record method, path, status and duration only: never bodies or document
  contents. Audit details hold IDs and labels only.
- Server timeouts set (including `ReadHeaderTimeout`); panics recovered.
