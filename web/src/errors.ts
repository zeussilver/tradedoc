import type { ApiError } from './api'
import { plural } from './format'

// errorHint adds a sentence saying what to do next, for the errors a
// reviewer can fix themselves. The server's own message is still shown
// above it; this only explains the workflow rule behind the error.
export function errorHint(e: ApiError): string | null {
  switch (e.code) {
    case 'line_already_accepted':
      return (
        'This invoice line already has an accepted candidate code, and a line takes one code. ' +
        'Reject the other candidates instead.'
      )
    case 'document_too_large':
      return (
        "The model can't read this file in one pass. Upload only the pages that hold the document itself. " +
        "The attempt still counts towards today's model limit."
      )
    case 'too_large':
      return 'The result is too large to store. Try a shorter document, or a shorter note.'
    case 'not_ready':
      return (
        'Checks and candidate codes always use the newest file of each type, ' +
        'so extract fields from a newly uploaded file first.'
      )
    case 'stale_check_run':
      if (e.info.reason === 'readings_changed') {
        return (
          'The documents were read again, or a newer file was uploaded, after this check run. ' +
          'Run the checks again and review the new findings before signing off.'
        )
      }
      if (e.info.reason === 'newer_run') {
        return 'The checks were run again after this page loaded. Review the newest run, now shown, before signing off.'
      }
      return 'This check run is out of date. Review the newest run before signing off.'
    case 'suggestions_stale':
      return (
        'The candidate codes were made from an earlier reading of the invoice. ' +
        'Suggest codes again and review them before approving.'
      )
    case 'suggestions_pending': {
      const n = e.info.pending
      return typeof n === 'number'
        ? `${plural(n, 'candidate code')} from this invoice reading ${n === 1 ? 'needs' : 'need'} a decision.`
        : 'Every candidate code from this invoice reading needs a decision before approval.'
    }
    default:
      return null
  }
}
