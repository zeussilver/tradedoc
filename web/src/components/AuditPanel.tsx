import type { AuditEvent } from '../api'
import { detailPairs, formatTime } from '../format'
import { PanelTitle } from './PanelTitle'

// AuditPanel lists every recorded action on the shipment, newest first.
// The server keeps details to IDs, counts and short labels, never document
// contents, so this table is safe to show and to ship to CloudWatch.
export function AuditPanel({ events }: { events: AuditEvent[] }) {
  return (
    <section className="panel" aria-labelledby="audit-h">
      <PanelTitle id="audit-h" step={5}>
        Audit trail
      </PanelTitle>
      {events.length === 0 ? (
        <p className="muted">No events yet.</p>
      ) : (
        <div className="table-wrap">
          <table className="data audit">
            <caption className="sr-only">Audit events, newest first</caption>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Entity</th>
                <th scope="col">Detail</th>
              </tr>
            </thead>
            <tbody>
              {[...events].reverse().map((e) => (
                <tr key={e.id}>
                  <td className="nowrap">{formatTime(e.at)}</td>
                  <td className="nowrap">{e.actor}</td>
                  <td className="nowrap">
                    <code>{e.action}</code>
                  </td>
                  <td>
                    {e.entity_type} <code className="id">{e.entity_id}</code>
                  </td>
                  <td>
                    <code className="detail">{detailPairs(e.detail)}</code>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
