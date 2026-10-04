import type { Confidence, Evidence, Field } from '../api'
import { Badge, type Tone } from './Badge'

const confidenceTone: Record<Confidence, Tone> = { high: 'neutral', medium: 'warning', low: 'error' }

// The badge shows the model's own high/medium/low rating. It is a prompt to
// look closer, not a measurement, so low values are the ones highlighted.
export function ConfidenceBadge({ level }: { level: Confidence }) {
  return (
    <Badge tone={confidenceTone[level]} title="The model's own rating of this value. It is not calibrated.">
      {level}
    </Badge>
  )
}

// EvidenceQuote shows where a value was read: the page, and on demand the
// exact text copied from the document.
export function EvidenceQuote({ evidence }: { evidence: Evidence }) {
  return (
    <details className="evidence">
      <summary>
        <span className="sr-only">Evidence on </span>p.{evidence.page}
      </summary>
      <blockquote>{evidence.quote}</blockquote>
    </details>
  )
}

// FieldValue renders one extracted value with its confidence and evidence.
// null is shown in words: "not found" is information, not an empty cell.
export function FieldValue<T>({ field, format }: { field: Field<T>; format?: (v: T) => string }) {
  if (field.value === null || field.value === undefined) {
    return <span className="not-found">not found in document</span>
  }
  return (
    <span className="field">
      <span className="field-value">{format ? format(field.value) : String(field.value)}</span>
      {field.confidence && <ConfidenceBadge level={field.confidence} />}
      {field.evidence && <EvidenceQuote evidence={field.evidence} />}
    </span>
  )
}
