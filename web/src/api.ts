// Typed client for the Go API. The types mirror the JSON tags in
// internal/domain/*.go exactly, so a renamed Go field shows up here as a
// type error instead of a blank cell in the UI.

// ---- shared value types (internal/domain/field.go) ----

export type Confidence = 'high' | 'medium' | 'low'

export interface Evidence {
  page: number // 1-based
  quote: string // exact text copied from the document
}

// A value of null means "not found in the document". Confidence and
// evidence are omitted by the server when the value is null.
export interface Field<T> {
  value: T | null
  confidence?: Confidence
  evidence?: Evidence
}

// ---- shipments and documents (internal/domain/models.go) ----

export interface Shipment {
  id: string
  reference: string
  created_at: string
}

export type DocType = 'commercial_invoice' | 'packing_list'
export type DocStatus = 'pending_upload' | 'uploaded' | 'extracted' | 'rejected'

// Mirrors domain.Document. Renamed because the browser already has a global
// type called Document (the HTML page).
export interface DocumentMeta {
  id: string
  shipment_id: string
  type: DocType
  filename: string
  content_type: string
  size_bytes: number
  sha256: string
  status: DocStatus
  uploaded_at: string
}

// ---- extractions (internal/domain/extraction.go) ----

export interface ModelRun {
  model: string
  attempts: number
  input_tokens: number
  output_tokens: number
}

// Money is an integer in the currency's minor unit (cents); weights are
// integer grams. Quantities and unit prices are the decimal string printed
// on the document without separators ("1250.5", "0.385"), so goods sold by
// weight or length keep their fractions and nothing is rounded.
export interface InvoiceLine {
  line_no: number
  description: Field<string>
  quantity: Field<string>
  unit: Field<string>
  unit_price: Field<string>
  line_total_minor: Field<number>
  origin_country: Field<string>
}

export type ChargeKind = 'freight' | 'insurance' | 'discount' | 'other'

// A non-goods row printed between the goods lines and the total, such as
// "Ocean freight 450.00". The amount is always positive; a discount is
// subtracted from the total.
export interface InvoiceCharge {
  kind: ChargeKind
  label: Field<string>
  amount_minor: Field<number>
}

export interface InvoiceExtraction {
  invoice_number: Field<string>
  invoice_date: Field<string>
  seller_name: Field<string>
  buyer_name: Field<string>
  currency: Field<string> // null when only a symbol such as "$" is printed
  incoterm: Field<string>
  line_items: InvoiceLine[] | null // a Go nil slice arrives as null
  charges: InvoiceCharge[] | null
  total_minor: Field<number>
}

export interface PackingLine {
  line_no: number
  description: Field<string>
  quantity: Field<string> // decimal string, like InvoiceLine.quantity
  unit: Field<string>
  packages: Field<number>
  gross_weight_grams: Field<number>
}

export interface PackingListExtraction {
  packing_list_number: Field<string>
  invoice_ref: Field<string>
  line_items: PackingLine[] | null
  total_packages: Field<number>
  total_gross_weight_grams: Field<number>
}

export interface Extraction {
  id: string
  document_id: string
  source: string // "manual" or "model:<model>"
  run?: ModelRun
  created_at: string
  invoice?: InvoiceExtraction
  packing_list?: PackingListExtraction
}

// ---- checks ----

export type Severity = 'error' | 'warning' | 'info'

export interface Ref {
  document_id: string
  path: string
  value: string // display form, already formatted by the server
  evidence?: Evidence
}

export interface Finding {
  code: string
  severity: Severity
  message: string
  line_no?: number
  refs: Ref[] | null // a Go nil slice arrives as null
}

export interface CheckRun {
  id: string
  shipment_id: string
  invoice_extraction_id: string
  packing_list_extraction_id: string
  findings: Finding[] | null
  created_at: string
}

// ---- classification suggestions ----

export type SuggestionStatus = 'pending' | 'accepted' | 'rejected'

export interface Suggestion {
  id: string
  shipment_id: string
  document_id: string
  extraction_id: string
  line_no: number
  description: string
  evidence?: Evidence
  rank: number
  hs_code: string
  rationale: string
  confidence: Confidence
  missing_info: string[] | null
  source: string
  status: SuggestionStatus
  reviewed_by?: string
  review_note?: string
  reviewed_at?: string
  created_at: string
}

// ---- sign-off and audit ----

export type SignOffDecision = 'approved' | 'changes_requested'

export interface SignOff {
  id: string
  shipment_id: string
  check_run_id: string
  decision: SignOffDecision
  reviewer: string
  note?: string
  open_errors: number
  created_at: string
}

export interface AuditEvent {
  id: string
  at: string
  actor: string
  action: string
  entity_type: string
  entity_id: string
  shipment_id: string
  detail?: Record<string, string>
}

// ---- config and uploads ----

export interface Config {
  model_enabled: boolean
  model: string
  max_upload_bytes: number
  max_image_bytes: number
  allowed_types: string[]
  // Currencies whose minor unit is not 2 decimals, e.g. {"JPY": 0, "KWD": 3}.
  // The checks use the same table, so amounts read the same everywhere.
  currency_minor_units: Record<string, number>
}

export interface PresignedUpload {
  url: string
  method: string // always PUT
  headers: Record<string, string> // part of the signature: send exactly as given
  expires_at: string
}

// ---- errors ----

export interface FieldError {
  path: string
  message: string
}

// ApiError carries the server's {"error": {code, message, details}} so the
// UI can show it next to the button that failed. "details" is either a list
// of field problems (validation) or an object of facts about the error,
// e.g. {"reason": "readings_changed"} for a stale check run.
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly details: FieldError[]
  readonly info: Record<string, unknown>
  readonly retryAfterSeconds: number | null

  constructor(
    status: number,
    code: string,
    message: string,
    details: FieldError[] = [],
    retryAfterSeconds: number | null = null,
    info: Record<string, unknown> = {},
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
    this.info = info
    this.retryAfterSeconds = retryAfterSeconds
  }
}

// asApiError turns anything thrown into an ApiError, so components only
// handle one error type.
export function asApiError(e: unknown): ApiError {
  if (e instanceof ApiError) return e
  return new ApiError(0, 'client_error', e instanceof Error ? e.message : 'Something went wrong.')
}

function isFieldError(v: unknown): v is FieldError {
  return (
    typeof v === 'object' &&
    v !== null &&
    typeof (v as FieldError).path === 'string' &&
    typeof (v as FieldError).message === 'string'
  )
}

export async function errorFromResponse(res: Response): Promise<ApiError> {
  const retry = Number.parseInt(res.headers.get('Retry-After') ?? '', 10)
  const retryAfter = Number.isFinite(retry) ? retry : null
  try {
    const body = (await res.json()) as { error?: { code?: string; message?: string; details?: unknown } }
    if (body.error?.code) {
      const raw = body.error.details
      const details = Array.isArray(raw) ? raw.filter(isFieldError) : []
      const isObject = typeof raw === 'object' && raw !== null && !Array.isArray(raw)
      const info = isObject ? (raw as Record<string, unknown>) : {}
      return new ApiError(res.status, body.error.code, body.error.message ?? res.statusText, details, retryAfter, info)
    }
  } catch {
    // Not our JSON error shape (e.g. a proxy's HTML error page): fall through.
  }
  return new ApiError(res.status, 'http_error', `The server answered HTTP ${res.status}.`, [], retryAfter)
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  const init: RequestInit = { method, headers }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  let res: Response
  try {
    res = await fetch(path, init)
  } catch {
    throw new ApiError(0, 'network_error', 'Could not reach the server. Check the connection and try again.')
  }
  if (!res.ok) throw await errorFromResponse(res)
  return (await res.json()) as T
}

// IDs come from the URL bar, so they are encoded before going into a path:
// a crafted "id" must not be able to reach a different API route.
const seg = encodeURIComponent

// ---- endpoints ----

export const api = {
  getConfig: () => request<Config>('GET', '/v1/config'),

  createShipment: (reference: string) => request<Shipment>('POST', '/v1/shipments', { reference }),
  getShipment: (id: string) => request<Shipment>('GET', `/v1/shipments/${seg(id)}`),

  listDocuments: async (shipmentId: string) =>
    (await request<{ documents: DocumentMeta[] | null }>('GET', `/v1/shipments/${seg(shipmentId)}/documents`))
      .documents ?? [],
  createUpload: (
    shipmentId: string,
    body: { type: DocType; filename: string; content_type: string; size_bytes: number },
  ) =>
    request<{ document: DocumentMeta; upload: PresignedUpload }>(
      'POST',
      `/v1/shipments/${seg(shipmentId)}/uploads`,
      body,
    ),
  completeUpload: (documentId: string) => request<DocumentMeta>('POST', `/v1/documents/${seg(documentId)}/complete`),

  extract: (documentId: string) => request<Extraction>('POST', `/v1/documents/${seg(documentId)}/extract`),
  listExtractions: async (documentId: string) =>
    (await request<{ extractions: Extraction[] | null }>('GET', `/v1/documents/${seg(documentId)}/extractions`))
      .extractions ?? [],

  runChecks: (shipmentId: string) => request<CheckRun>('POST', `/v1/shipments/${seg(shipmentId)}/checks`),
  listCheckRuns: async (shipmentId: string) =>
    (await request<{ check_runs: CheckRun[] | null }>('GET', `/v1/shipments/${seg(shipmentId)}/checks`)).check_runs ??
    [],

  suggest: async (shipmentId: string) =>
    (await request<{ suggestions: Suggestion[] | null }>('POST', `/v1/shipments/${seg(shipmentId)}/suggestions`))
      .suggestions ?? [],
  listSuggestions: async (shipmentId: string) =>
    (await request<{ suggestions: Suggestion[] | null }>('GET', `/v1/shipments/${seg(shipmentId)}/suggestions`))
      .suggestions ?? [],
  reviewSuggestion: (id: string, body: { decision: 'accepted' | 'rejected'; reviewer: string; note: string }) =>
    request<Suggestion>('POST', `/v1/suggestions/${seg(id)}/review`, body),

  signOff: (
    shipmentId: string,
    body: { decision: SignOffDecision; reviewer: string; check_run_id: string; note: string },
  ) => request<SignOff>('POST', `/v1/shipments/${seg(shipmentId)}/signoffs`, body),
  listSignOffs: async (shipmentId: string) =>
    (await request<{ signoffs: SignOff[] | null }>('GET', `/v1/shipments/${seg(shipmentId)}/signoffs`)).signoffs ?? [],

  listAudit: async (shipmentId: string) =>
    (await request<{ events: AuditEvent[] | null }>('GET', `/v1/shipments/${seg(shipmentId)}/audit`)).events ?? [],
}
