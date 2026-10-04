import type { CheckRun, DocType, DocumentMeta, Extraction } from './api'

// These rules decide which reading of each document is "current", and they
// match the server's: checks and candidate codes always use the newest
// reading of the NEWEST document of each type. If that document has not
// been read yet (a corrected invoice was just uploaded, say), the server
// answers 409 not_ready instead of quietly using an older file, so the
// buttons here are disabled for the same reason.

// newestOfType picks the last document of a type, whatever its status. The
// API lists documents oldest first, so a re-upload replaces the one shown in
// the card, even before it has been read.
export function newestOfType(docs: DocumentMeta[], type: DocType): DocumentMeta | undefined {
  return docs.filter((d) => d.type === type).at(-1)
}

// isRead reports whether the newest document of a type has been read.
export function isRead(docs: DocumentMeta[], type: DocType): boolean {
  return newestOfType(docs, type)?.status === 'extracted'
}

// Readings holds the extraction ID that is current for each document type,
// or undefined when the newest document of that type has no reading.
export interface Readings {
  invoice?: string
  packingList?: string
}

// currentReadings finds the newest reading of the newest document of each
// type. extractions maps a document ID to its readings, oldest first.
export function currentReadings(docs: DocumentMeta[], extractions: Record<string, Extraction[]>): Readings {
  const newestReading = (type: DocType) => {
    const doc = newestOfType(docs, type)
    return doc?.status === 'extracted' ? extractions[doc.id]?.at(-1)?.id : undefined
  }
  return { invoice: newestReading('commercial_invoice'), packingList: newestReading('packing_list') }
}

// runIsCurrent reports whether a check run compared the readings that are
// current now. If either document was read again, or a newer file was
// uploaded, the run's findings describe something no longer on screen, and
// the server refuses to sign it off (409 stale_check_run).
export function runIsCurrent(run: CheckRun, current: Readings): boolean {
  return run.invoice_extraction_id === current.invoice && run.packing_list_extraction_id === current.packingList
}
