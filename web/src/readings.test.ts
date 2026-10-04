import { describe, expect, it } from 'vitest'
import type { CheckRun, DocumentMeta, Extraction } from './api'
import { currentReadings, isRead, newestOfType, runIsCurrent } from './readings'

const doc = (id: string, type: DocumentMeta['type'], status: DocumentMeta['status']): DocumentMeta => ({
  id,
  shipment_id: 'shp_1',
  type,
  filename: `${id}.pdf`,
  content_type: 'application/pdf',
  size_bytes: 1000,
  sha256: '',
  status,
  uploaded_at: '2026-10-04T01:00:00Z',
})
const ext = (id: string, documentId: string): Extraction => ({
  id,
  document_id: documentId,
  source: 'model:fake',
  created_at: '2026-10-04T01:00:00Z',
})
const run = (inv: string, pl: string): CheckRun => ({
  id: 'chk_1',
  shipment_id: 'shp_1',
  invoice_extraction_id: inv,
  packing_list_extraction_id: pl,
  findings: [],
  created_at: '2026-10-04T01:00:00Z',
})

// Both documents read once, and the checks run on those readings.
const docs = [doc('inv_a', 'commercial_invoice', 'extracted'), doc('pl_a', 'packing_list', 'extracted')]
const extractions = { inv_a: [ext('ext_inv1', 'inv_a')], pl_a: [ext('ext_pl1', 'pl_a')] }

describe('current readings', () => {
  it('starts out current', () => {
    const cur = currentReadings(docs, extractions)
    expect(cur).toEqual({ invoice: 'ext_inv1', packingList: 'ext_pl1' })
    expect(isRead(docs, 'commercial_invoice')).toBe(true)
    expect(runIsCurrent(run('ext_inv1', 'ext_pl1'), cur)).toBe(true)
  })

  it('makes a run stale when a document is read again', () => {
    const again = { ...extractions, inv_a: [ext('ext_inv1', 'inv_a'), ext('ext_inv2', 'inv_a')] }
    const cur = currentReadings(docs, again)
    expect(cur.invoice).toBe('ext_inv2')
    expect(runIsCurrent(run('ext_inv1', 'ext_pl1'), cur)).toBe(false)
  })

  // A corrected invoice was uploaded but not read yet. The old reading must
  // not count as current: the server refuses checks (not_ready) and
  // sign-off (stale_check_run) until the new file is read.
  it.each(['uploaded', 'pending_upload', 'rejected'] as const)('a newer %s invoice is not read', (status) => {
    const withNew = [...docs, doc('inv_b', 'commercial_invoice', status)]
    expect(newestOfType(withNew, 'commercial_invoice')?.id).toBe('inv_b')
    expect(isRead(withNew, 'commercial_invoice')).toBe(false)
    const cur = currentReadings(withNew, extractions)
    expect(cur.invoice).toBeUndefined()
    expect(cur.packingList).toBe('ext_pl1')
    expect(runIsCurrent(run('ext_inv1', 'ext_pl1'), cur)).toBe(false)
  })

  it('has nothing current before any upload', () => {
    expect(isRead([], 'packing_list')).toBe(false)
    expect(currentReadings([], {})).toEqual({ invoice: undefined, packingList: undefined })
  })
})
