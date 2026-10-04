import { useId, useState, type FormEvent } from 'react'
import { api, type CheckRun, type SignOff, type SignOffDecision } from '../api'
import { formatTime, plural } from '../format'
import { useAction } from '../hooks'
import { Badge } from './Badge'
import { ErrorMessage } from './ErrorMessage'
import { ReviewerField } from './ReviewerField'
import { PanelTitle } from './PanelTitle'

const decisionLabel: Record<SignOffDecision, string> = {
  approved: 'Approved',
  changes_requested: 'Changes requested',
}

interface Props {
  shipmentId: string
  latestRun?: CheckRun
  runCurrent: boolean // latestRun compared the readings that are current now
  signOffs: SignOff[] // oldest first
  pendingSuggestions: number // undecided candidates from the run's invoice reading
  suggestionsStale: boolean // candidates exist, but none from the run's invoice reading
  reviewer: string
  onReviewerChange: (name: string) => void
  onChanged: () => void
}

// SignOffPanel is the human step the workflow ends with: a named person
// approves the shipment, or sends it back, after reading a specific check run.
export function SignOffPanel(props: Props) {
  const { shipmentId, latestRun, runCurrent, signOffs, pendingSuggestions, suggestionsStale } = props
  const { reviewer, onReviewerChange, onChanged } = props
  const action = useAction()
  const [decision, setDecision] = useState<SignOffDecision>('approved')
  const [note, setNote] = useState('')
  const noteId = useId()

  // The approval hints only apply while approving is possible at all.
  const approving = runCurrent && decision === 'approved'

  const findings = latestRun?.findings ?? []
  const errors = findings.filter((f) => f.severity === 'error').length
  const warnings = findings.filter((f) => f.severity === 'warning').length

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (!latestRun) return
    // The run ID is sent so the server can refuse (409) if someone ran the
    // checks again after this page was loaded.
    const ok = await action.run(() =>
      api.signOff(shipmentId, { decision, reviewer: reviewer.trim(), check_run_id: latestRun.id, note: note.trim() }),
    )
    if (ok) setNote('')
    onChanged()
  }

  return (
    <section className="panel" aria-labelledby="signoff-h">
      <PanelTitle id="signoff-h" step={4}>
        Sign-off
      </PanelTitle>
      {!latestRun ? (
        <p className="muted">Run the cross-document checks first: a sign-off always refers to a specific check run.</p>
      ) : (
        <form className="signoff-form" onSubmit={onSubmit}>
          <p className="run-info">
            Signs off check run <code className="id">{latestRun.id}</code> from {formatTime(latestRun.created_at)}:{' '}
            {plural(errors, 'error')}, {plural(warnings, 'warning')}.
          </p>
          {/* A run that compared older readings than the ones on screen can't
              be signed off either way: the server answers 409 stale_check_run.
              So the decision and the button are disabled until the checks run again. */}
          {!runCurrent && (
            <p className="notice notice-warning">
              <strong>This run compared older readings than the ones shown above.</strong> Run the checks again
              before signing off.
            </p>
          )}
          <ReviewerField value={reviewer} onChange={onReviewerChange} />
          <fieldset className="decision" disabled={!runCurrent}>
            <legend>Decision</legend>
            {(Object.keys(decisionLabel) as SignOffDecision[]).map((d) => (
              <label key={d} className="radio">
                <input
                  type="radio"
                  name="decision"
                  value={d}
                  checked={decision === d}
                  onChange={() => {
                    setDecision(d)
                    action.setError(null) // an error about the other decision no longer applies
                  }}
                />{' '}
                {decisionLabel[d]}
              </label>
            ))}
          </fieldset>
          <div className="form-row">
            <label htmlFor={noteId}>Note{approving && errors > 0 ? ' (required)' : ' (optional)'}</label>
            <textarea id={noteId} rows={3} maxLength={500} value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          {approving && errors > 0 && (
            <p className="hint">
              This run has {plural(errors, 'error finding')}. Approving needs a note explaining why the shipment can
              proceed anyway.
            </p>
          )}
          {approving && pendingSuggestions > 0 && (
            <p className="hint">
              {plural(pendingSuggestions, 'candidate code')} still awaiting review. Accept or reject them before
              approving.
            </p>
          )}
          {approving && suggestionsStale && (
            <p className="hint">
              The candidate codes were made from an earlier reading of the invoice. Suggest codes again and review
              them before approving.
            </p>
          )}
          <div className="actions">
            <button type="submit" disabled={!reviewer.trim() || !runCurrent || action.busy}>
              {action.busy ? 'Recording…' : 'Record sign-off'}
            </button>
            {!reviewer.trim() && <span className="hint">Enter your name first.</span>}
          </div>
          <ErrorMessage error={action.error} />
        </form>
      )}

      {signOffs.length > 0 && (
        <div className="table-wrap">
          <table className="data">
            <caption className="sr-only">Sign-off history, newest first</caption>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Decision</th>
                <th scope="col">Reviewer</th>
                <th scope="col">Check run</th>
                <th scope="col" className="num">
                  Open errors
                </th>
                <th scope="col">Note</th>
              </tr>
            </thead>
            <tbody>
              {[...signOffs].reverse().map((s) => (
                <tr key={s.id}>
                  <td className="nowrap">{formatTime(s.created_at)}</td>
                  <td>
                    <Badge tone={s.decision === 'approved' ? 'ok' : 'warning'}>{decisionLabel[s.decision]}</Badge>
                  </td>
                  <td className="nowrap">{s.reviewer}</td>
                  <td>
                    <code className="id">{s.check_run_id}</code>
                    {latestRun && s.check_run_id !== latestRun.id && <span className="muted"> (earlier run)</span>}
                  </td>
                  <td className="num">{s.open_errors}</td>
                  <td>{s.note}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
