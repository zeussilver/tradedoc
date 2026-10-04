import { useElapsed } from '../hooks'

// Elapsed shows "Reading the document… 23 s" while a long call runs.
//
// The status element is always on the page and only its text changes:
// screen readers announce changes inside a live region that already exists,
// but often stay silent about one that appears with its text already in it.
// The ticking counter is kept outside the live region (and hidden from
// screen readers), so they hear the label once, not every second.
export function Elapsed({ running, label, hint }: { running: boolean; label: string; hint?: string }) {
  const seconds = useElapsed(running)
  return (
    <span className="busy">
      <span role="status">{running ? label : ''}</span>
      {running && (
        <>
          {' '}
          <span aria-hidden="true">{seconds} s</span>
          {hint && <span className="hint"> ({hint})</span>}
        </>
      )}
    </span>
  )
}
