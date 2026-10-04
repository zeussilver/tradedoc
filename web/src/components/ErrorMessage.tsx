import type { ApiError } from '../api'
import { errorHint } from '../errors'
import { formatWait } from '../format'

// ErrorMessage shows an API error next to the action that caused it, with
// a hint on what to do next for workflow errors, and each validation
// problem as "path: message".
export function ErrorMessage({ error }: { error: ApiError | null }) {
  if (!error) return null
  const hint = errorHint(error)
  return (
    <div className="error" role="alert">
      <p>
        <strong>{error.message}</strong>{' '}
        <span className="error-code">
          ({error.code}
          {error.status > 0 ? `, HTTP ${error.status}` : ''})
        </span>
      </p>
      {hint && <p>{hint}</p>}
      {error.retryAfterSeconds !== null && <p>Try again in {formatWait(error.retryAfterSeconds)}.</p>}
      {error.details.length > 0 && (
        <ul>
          {error.details.map((d, i) => (
            <li key={i}>
              <code>{d.path}</code>: {d.message}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
