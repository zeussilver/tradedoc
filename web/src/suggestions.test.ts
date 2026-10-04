import { describe, expect, it } from 'vitest'
import type { Suggestion } from './api'
import { acceptedLines, groupSuggestions, lineKey, pendingFor } from './suggestions'

const base: Suggestion = {
  id: '',
  shipment_id: 'shp_1',
  document_id: 'doc_1',
  extraction_id: 'ext_1',
  line_no: 1,
  description: 'Porcelain dinner plate',
  rank: 1,
  hs_code: '691110',
  rationale: 'r',
  confidence: 'medium',
  missing_info: null,
  source: 'model:fake',
  status: 'pending',
  created_at: '2026-10-04T01:00:00.1Z',
}
const s = (over: Partial<Suggestion>): Suggestion => ({ ...base, ...over })

describe('groupSuggestions', () => {
  it('groups by batch, then line, then rank', () => {
    const batches = groupSuggestions([
      s({ id: 'a2', line_no: 2, rank: 1 }),
      s({ id: 'a1b', line_no: 1, rank: 2 }),
      s({ id: 'a1a', line_no: 1, rank: 1 }),
      s({ id: 'b1', extraction_id: 'ext_2', created_at: '2026-10-04T02:00:00Z' }),
    ])
    expect(batches.map((b) => b.extractionId)).toEqual(['ext_2', 'ext_1']) // newest first
    const older = batches[1]!
    expect(older.lines.map((l) => l.lineNo)).toEqual([1, 2])
    expect(older.lines[0]!.candidates.map((c) => c.id)).toEqual(['a1a', 'a1b'])
  })

  it('keeps two batches from the same reading in the same second apart', () => {
    // Pressing "Suggest again" quickly: same reading, same second, but a
    // different full timestamp. Merging them would put two rank-1
    // candidates on one line.
    const batches = groupSuggestions([
      s({ id: 'first', created_at: '2026-10-04T01:00:00.1Z' }),
      s({ id: 'second', created_at: '2026-10-04T01:00:00.9Z' }),
    ])
    expect(batches.map((b) => b.lines[0]!.candidates.map((c) => c.id))).toEqual([['second'], ['first']])
  })

  it('orders batches by the API order, not by comparing timestamp strings', () => {
    // Go drops trailing zeros from fractional seconds, so as strings
    // "…00.1Z" > "…00.09Z" although .09 is earlier. The API lists the
    // .09 batch first because it was stored first; the newest is shown first.
    const batches = groupSuggestions([
      s({ id: 'early', extraction_id: 'ext_a', created_at: '2026-10-04T01:00:00.09Z' }),
      s({ id: 'late', extraction_id: 'ext_b', created_at: '2026-10-04T01:00:00.1Z' }),
    ])
    expect(batches.map((b) => b.extractionId)).toEqual(['ext_b', 'ext_a'])

    const reversed = groupSuggestions([
      s({ id: 'late', extraction_id: 'ext_b', created_at: '2026-10-04T01:00:00.1Z' }),
      s({ id: 'early', extraction_id: 'ext_a', created_at: '2026-10-04T01:00:00.09Z' }),
    ])
    expect(reversed.map((b) => b.extractionId)).toEqual(['ext_a', 'ext_b'])
  })
})

describe('accepted lines and pending counts', () => {
  const list = [
    s({ id: 'l1r1', line_no: 1, rank: 1, status: 'accepted' }),
    s({ id: 'l1r2', line_no: 1, rank: 2 }),
    s({ id: 'l2r1', line_no: 2, rank: 1 }),
    // The same line of an older reading is a different line.
    s({ id: 'old', extraction_id: 'ext_0', line_no: 2, status: 'accepted' }),
    s({ id: 'oldp', extraction_id: 'ext_0', line_no: 3 }),
  ]

  it('knows which lines already have an accepted code', () => {
    const accepted = acceptedLines(list)
    expect(accepted.has(lineKey({ extraction_id: 'ext_1', line_no: 1 }))).toBe(true)
    // Line 2 of ext_1 is still open, even though line 2 of ext_0 was accepted.
    expect(accepted.has(lineKey({ extraction_id: 'ext_1', line_no: 2 }))).toBe(false)
    expect(accepted.has(lineKey({ extraction_id: 'ext_0', line_no: 2 }))).toBe(true)
  })

  it('counts pending candidates of one reading only', () => {
    expect(pendingFor(list, 'ext_1')).toBe(2)
    expect(pendingFor(list, 'ext_0')).toBe(1)
    expect(pendingFor(list, undefined)).toBe(0)
  })
})
