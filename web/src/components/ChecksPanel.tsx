import { api, type CheckRun, type DocType, type Finding, type Severity } from '../api'
import { docTypeLabel, formatTime, plural } from '../format'
import { useAction } from '../hooks'
import type { Readings } from '../readings'
import { Badge } from './Badge'
import { ErrorMessage } from './ErrorMessage'
import { PanelTitle } from './PanelTitle'

const SEVERITIES: { severity: Severity; heading: string }[] = [
  { severity: 'error', heading: 'Errors' },
  { severity: 'warning', heading: 'Warnings' },
  { severity: 'info', heading: 'Info' },
]

interface Props {
  shipmentId: string
  runs: CheckRun[] // oldest first
  docTypes: Map<string, DocType> // document ID -> type, to label each Ref
  ready: boolean // the newest invoice and the newest packing list have both been read
  current: Readings | null // the current readings; null while the documents load
  runCurrent: boolean // the newest run compared the current readings
  onChanged: () => void
}

export function ChecksPanel({ shipmentId, runs, docTypes, ready, current, runCurrent, onChanged }: Props) {
  const action = useAction()
  const latest = runs.at(-1)

  async function onRun() {
    await action.run(() => api.runChecks(shipmentId))
    onChanged()
  }

  return (
    <section className="panel" aria-labelledby="checks-h">
      <PanelTitle id="checks-h" step={2}>
        Cross-document checks
      </PanelTitle>
      <p className="panel-intro">
        Compares the newest reading of the newest invoice with the newest reading of the newest packing list, line by
        line: invoice reference, descriptions, units, quantities, line and invoice totals, charges, packages and gross
        weight. Values are compared exactly as read; nothing is corrected.
      </p>
      <div className="actions">
        <button type="button" onClick={onRun} disabled={!ready || action.busy}>
          {action.busy ? 'Running…' : latest ? 'Run checks again' : 'Run checks'}
        </button>
        {!ready && <span className="hint">Extract fields from the newest invoice and packing list first.</span>}
      </div>
      <ErrorMessage error={action.error} />
      {latest ? (
        <CheckRunView
          run={latest}
          earlier={runs.length - 1}
          docTypes={docTypes}
          current={current}
          runCurrent={runCurrent}
        />
      ) : (
        <p className="muted">No checks run yet.</p>
      )}
    </section>
  )
}

interface RunProps {
  run: CheckRun
  earlier: number
  docTypes: Map<string, DocType>
  current: Readings | null
  runCurrent: boolean
}

function CheckRunView({ run, earlier, docTypes, current, runCurrent }: RunProps) {
  const findings = run.findings ?? []
  // Says whether each reading the run used is still the current one. While
  // the documents are loading (current is null) nothing is claimed.
  const note = (id: string, currentId: string | undefined) =>
    !current ? '' : id === currentId ? ' (current)' : ' (not the current reading)'
  return (
    <div className="check-run">
      <p className="run-info">
        Run {formatTime(run.created_at)} · <code className="id">{run.id}</code>
        {earlier > 0 && ` · ${plural(earlier, 'earlier run')} not shown`}
      </p>
      {/* The reading IDs match the ones shown under each document, so a
          reviewer can see exactly which readings these findings are about. */}
      <p className="run-info">
        Compared invoice reading <code className="id">{run.invoice_extraction_id}</code>
        {note(run.invoice_extraction_id, current?.invoice)} with packing list reading{' '}
        <code className="id">{run.packing_list_extraction_id}</code>
        {note(run.packing_list_extraction_id, current?.packingList)}.
      </p>
      {!runCurrent && (
        <p className="notice notice-warning">
          <strong>The documents changed after this run.</strong> A document was read again or a newer file was
          uploaded, so these findings may not describe what is shown above. Run the checks again before signing off.
        </p>
      )}
      {findings.length === 0 && <p>No findings in this run.</p>}
      {SEVERITIES.map(({ severity, heading }) => {
        const items = findings.filter((f) => f.severity === severity)
        if (items.length === 0) return null
        return (
          <div key={severity} className="severity-group">
            <h3 className={`severity-heading tone-text-${severity}`}>
              {heading} ({items.length})
            </h3>
            <ul className="findings">
              {items.map((f, i) => (
                <FindingItem key={`${f.code}-${f.line_no ?? 0}-${i}`} finding={f} docTypes={docTypes} />
              ))}
            </ul>
          </div>
        )
      })}
    </div>
  )
}

function FindingItem({ finding, docTypes }: { finding: Finding; docTypes: Map<string, DocType> }) {
  const refs = finding.refs ?? []
  return (
    <li className={`finding finding-${finding.severity}`}>
      <div className="finding-head">
        <Badge tone={finding.severity}>{finding.severity}</Badge>
        {finding.line_no ? <span className="line-tag">Line {finding.line_no}</span> : null}
        <span className="finding-message">{finding.message}</span>
        <code className="finding-code">{finding.code}</code>
      </div>
      {refs.length > 0 && (
        <div className="table-wrap">
          <table className="data refs">
            <caption className="sr-only">Values this finding refers to</caption>
            <thead>
              <tr>
                <th scope="col">Document</th>
                <th scope="col">Path</th>
                <th scope="col">Value</th>
                <th scope="col">Evidence</th>
              </tr>
            </thead>
            <tbody>
              {refs.map((r, i) => {
                const type = docTypes.get(r.document_id)
                return (
                  <tr key={i}>
                    <td className="nowrap">{type ? docTypeLabel(type) : 'Document'}</td>
                    <td>
                      <code>{r.path}</code>
                    </td>
                    <td className="nowrap">{r.value}</td>
                    <td>
                      {r.evidence ? (
                        <>
                          <span className="page">p.{r.evidence.page}</span> <q className="quote">{r.evidence.quote}</q>
                        </>
                      ) : (
                        <span className="muted">none</span>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </li>
  )
}
