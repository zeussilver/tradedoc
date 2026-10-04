import { useId, useState } from 'react'
import { api, type Config, type Suggestion, type SuggestionStatus } from '../api'
import { formatHS, formatTime, plural } from '../format'
import { useAction } from '../hooks'
import type { Readings } from '../readings'
import { acceptedLines, groupSuggestions, lineKey, pendingFor, type SuggestionLine } from '../suggestions'
import { Badge, type Tone } from './Badge'
import { Elapsed } from './Elapsed'
import { ErrorMessage } from './ErrorMessage'
import { ConfidenceBadge } from './FieldValue'
import { ReviewerField } from './ReviewerField'
import { PanelTitle } from './PanelTitle'

const statusTone: Record<SuggestionStatus, Tone> = { pending: 'warning', accepted: 'ok', rejected: 'neutral' }

interface Props {
  shipmentId: string
  config: Config | null
  suggestions: Suggestion[]
  ready: boolean // the newest invoice has been read
  current: Readings | null // the current readings; null while the documents load
  reviewer: string
  onReviewerChange: (name: string) => void
  onChanged: () => void
}

export function SuggestionsPanel({
  shipmentId,
  config,
  suggestions,
  ready,
  current,
  reviewer,
  onReviewerChange,
  onChanged,
}: Props) {
  const action = useAction()
  const modelOn = config?.model_enabled ?? false
  const batches = groupSuggestions(suggestions)
  const accepted = acceptedLines(suggestions)
  // A batch made from an older reading of the invoice describes lines that
  // may have changed, so it is kept as a record but can't be reviewed.
  // Nothing is marked while the documents are still loading.
  const superseded = (extractionId: string) => current !== null && extractionId !== current.invoice
  const reviewable = batches.filter((b) => !superseded(b.extractionId))
  const pending = pendingFor(suggestions, current?.invoice)

  async function onSuggest() {
    await action.run(() => api.suggest(shipmentId))
    onChanged()
  }

  return (
    <section className="panel" aria-labelledby="codes-h">
      <PanelTitle id="codes-h" step={3}>
        Candidate tariff codes
      </PanelTitle>
      <p className="notice notice-broker">
        <strong>Candidate codes for a licensed customs broker to review. Not a classification decision.</strong> Only
        each line's description and unit are sent to the model. Codes are 6-digit HS subheadings; the national tariff
        item is the broker's call.
      </p>
      <div className="actions">
        <button type="button" onClick={onSuggest} disabled={!modelOn || !ready || action.busy}>
          {batches.length > 0 ? 'Suggest again' : 'Suggest candidate codes'}
        </button>
        <Elapsed running={action.busy} label="Asking the model…" hint="usually 30–90 s" />
        {!modelOn && config && <span className="hint">The model is switched off on this server.</span>}
        {modelOn && !ready && <span className="hint">Extract fields from the newest invoice first.</span>}
      </div>
      <ErrorMessage error={action.error} />

      {reviewable.length > 0 && (
        <>
          <ReviewerField value={reviewer} onChange={onReviewerChange} />
          {current !== null && (
            <p className="hint">
              {pending > 0 ? `${plural(pending, 'candidate')} awaiting review. ` : 'All candidates reviewed. '}
              {pending > 0 && !reviewer.trim() && 'Enter your name to accept or reject candidates.'}
            </p>
          )}
        </>
      )}
      {batches.length > 0 && reviewable.length === 0 && current !== null && (
        <p className="hint">
          These candidate codes were made from an earlier reading of the invoice. Suggest codes again from the newest
          invoice reading.
        </p>
      )}

      {batches.map((b) => {
        const old = superseded(b.extractionId)
        return (
          <div key={b.key} className="batch">
            <p className="run-info">
              Suggested {formatTime(b.createdAt)} · {b.source} · from invoice reading{' '}
              <code className="id">{b.extractionId}</code>
              {old && (
                <>
                  {' '}
                  <Badge tone="neutral">superseded</Badge> made from an earlier reading of the invoice; kept as a
                  record and no longer reviewed
                </>
              )}
            </p>
            {b.lines.map((line) => (
              <LineGroup
                key={line.lineNo}
                line={line}
                reviewable={!old}
                lineAccepted={accepted.has(lineKey({ extraction_id: b.extractionId, line_no: line.lineNo }))}
                reviewer={reviewer}
                onChanged={onChanged}
              />
            ))}
          </div>
        )
      })}
    </section>
  )
}

interface LineProps {
  line: SuggestionLine
  reviewable: boolean // false for a superseded batch
  lineAccepted: boolean // some candidate for this line (in any batch from the same reading) is accepted
  reviewer: string
  onChanged: () => void
}

function LineGroup({ line, reviewable, lineAccepted, reviewer, onChanged }: LineProps) {
  return (
    <article className="line-group">
      <header className="line-head">
        <h3>
          <span className="line-tag">Line {line.lineNo}</span> {line.description}
        </h3>
        {line.evidence && (
          <p className="line-evidence">
            <span className="page">p.{line.evidence.page}</span> <q className="quote">{line.evidence.quote}</q>
          </p>
        )}
      </header>
      <ol className="candidates">
        {line.candidates.map((s) => (
          <Candidate
            key={s.id}
            s={s}
            reviewable={reviewable}
            lineAccepted={lineAccepted}
            reviewer={reviewer}
            onChanged={onChanged}
          />
        ))}
      </ol>
    </article>
  )
}

interface CandidateProps {
  s: Suggestion
  reviewable: boolean
  lineAccepted: boolean
  reviewer: string
  onChanged: () => void
}

function Candidate({ s, reviewable, lineAccepted, reviewer, onChanged }: CandidateProps) {
  const missing = s.missing_info ?? []
  return (
    <li className="candidate">
      <div className="candidate-head">
        <span className="hs-code">{formatHS(s.hs_code)}</span>
        <span className="muted">candidate {s.rank}</span>
        <ConfidenceBadge level={s.confidence} />
        <Badge tone={statusTone[s.status]}>{s.status}</Badge>
      </div>
      <p className="rationale">{s.rationale}</p>
      {missing.length > 0 && (
        <div className="missing-info">
          <span className="label">A broker would need to confirm:</span>
          <ul>
            {missing.map((m, i) => (
              <li key={i}>{m}</li>
            ))}
          </ul>
        </div>
      )}
      {s.status === 'pending' ? (
        reviewable ? (
          <ReviewControls s={s} canAccept={!lineAccepted} reviewer={reviewer} onChanged={onChanged} />
        ) : (
          <p className="review-record">Not reviewed.</p>
        )
      ) : (
        <p className="review-record">
          {s.status === 'accepted' ? 'Accepted' : 'Rejected'} by <strong>{s.reviewed_by}</strong> ·{' '}
          {formatTime(s.reviewed_at)}
          {s.review_note && <> · “{s.review_note}”</>}
        </p>
      )}
    </li>
  )
}

interface ReviewProps {
  s: Suggestion
  canAccept: boolean // false once another candidate for the same line is accepted
  reviewer: string
  onChanged: () => void
}

// ReviewControls records a decision on one candidate. An invoice line takes
// exactly one code, so once a sibling candidate is accepted only Reject is
// offered here (the server refuses a second acceptance with 409 anyway).
function ReviewControls({ s, canAccept, reviewer, onChanged }: ReviewProps) {
  const action = useAction()
  const [note, setNote] = useState('')
  const noteId = useId()
  const name = reviewer.trim()

  async function decide(decision: 'accepted' | 'rejected') {
    await action.run(() => api.reviewSuggestion(s.id, { decision, reviewer: name, note: note.trim() }))
    onChanged()
  }

  return (
    <>
      <div className="review">
        <label htmlFor={noteId} className="sr-only">
          Note on candidate {formatHS(s.hs_code)} for line {s.line_no} (optional)
        </label>
        <input
          id={noteId}
          type="text"
          placeholder="Note (optional)"
          maxLength={500}
          value={note}
          onChange={(e) => setNote(e.target.value)}
        />
        {canAccept && (
          <button type="button" onClick={() => decide('accepted')} disabled={!name || action.busy}>
            Accept
          </button>
        )}
        <button type="button" className="secondary" onClick={() => decide('rejected')} disabled={!name || action.busy}>
          Reject
        </button>
        {!canAccept && <span className="hint">Another candidate for this line is accepted.</span>}
      </div>
      <ErrorMessage error={action.error} />
    </>
  )
}
