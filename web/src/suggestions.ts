import type { Evidence, Suggestion } from './api'

// A batch is one press of "Suggest candidate codes": all candidates made
// from the same invoice reading at the same moment. Pressing it again (say
// after re-extracting) adds a new batch; old ones stay as a record.
export interface SuggestionBatch {
  key: string
  createdAt: string
  source: string
  extractionId: string
  lines: SuggestionLine[]
}

export interface SuggestionLine {
  lineNo: number
  description: string
  evidence?: Evidence
  candidates: Suggestion[] // ranked, 1 first
}

// groupSuggestions turns the flat list from the API into batches, newest
// first, each holding its invoice lines in order, each line holding its
// ranked candidates.
//
// The server stamps every candidate of one call with the same created_at,
// so the full timestamp (with its fractional seconds) plus the reading it
// came from identifies a batch. Batches keep the API's order, which is the
// order they were stored (oldest first), just reversed: timestamps are never
// compared as strings, because "…00.1Z" and "…00.09Z" sort the wrong way.
export function groupSuggestions(list: Suggestion[]): SuggestionBatch[] {
  const batches = new Map<string, SuggestionBatch>()
  for (const s of list) {
    const key = `${s.extraction_id}@${s.created_at}`
    let batch = batches.get(key)
    if (!batch) {
      batch = { key, createdAt: s.created_at, source: s.source, extractionId: s.extraction_id, lines: [] }
      batches.set(key, batch)
    }
    let line = batch.lines.find((l) => l.lineNo === s.line_no)
    if (!line) {
      line = { lineNo: s.line_no, description: s.description, evidence: s.evidence, candidates: [] }
      batch.lines.push(line)
    }
    line.candidates.push(s)
  }
  const out = [...batches.values()]
  for (const b of out) {
    b.lines.sort((a, z) => a.lineNo - z.lineNo)
    for (const l of b.lines) l.candidates.sort((a, z) => a.rank - z.rank)
  }
  // A Map keeps insertion order, so this is the API order, newest first.
  return out.reverse()
}

// lineKey names one invoice line of one reading. A line may have only one
// accepted candidate code, across every batch made from that reading (the
// server answers 409 line_already_accepted to a second one).
export function lineKey(s: Pick<Suggestion, 'extraction_id' | 'line_no'>): string {
  return `${s.extraction_id}#${s.line_no}`
}

// acceptedLines lists the lines that already have an accepted candidate.
export function acceptedLines(list: Suggestion[]): Set<string> {
  return new Set(list.filter((s) => s.status === 'accepted').map(lineKey))
}

// pendingFor counts the candidates from one invoice reading that still need
// a decision. Candidates from an older reading are a superseded record:
// approval waits only for those made from the reading being signed off.
export function pendingFor(list: Suggestion[], extractionId: string | undefined): number {
  return list.filter((s) => s.status === 'pending' && s.extraction_id === extractionId).length
}
