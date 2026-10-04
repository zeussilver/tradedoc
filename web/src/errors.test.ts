import { describe, expect, it } from 'vitest'
import { ApiError, errorFromResponse } from './api'
import { errorHint } from './errors'

const jsonError = (status: number, error: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify({ error }), { status, headers: { 'Content-Type': 'application/json', ...headers } })

describe('errorFromResponse', () => {
  it('keeps object details as info', async () => {
    const e = await errorFromResponse(
      jsonError(409, {
        code: 'stale_check_run',
        message: 'run the checks again',
        details: { latest_check_run_id: 'chk_2', reason: 'readings_changed' },
      }),
    )
    expect(e.code).toBe('stale_check_run')
    expect(e.info.reason).toBe('readings_changed')
    expect(e.details).toEqual([])
  })

  it('keeps list details as field problems', async () => {
    const e = await errorFromResponse(
      jsonError(409, {
        code: 'not_ready',
        message: 'checks need the newest invoice and the newest packing list to be read',
        details: [{ path: 'commercial_invoice', message: 'the newest invoice has not been read yet' }],
      }),
    )
    expect(e.details).toEqual([{ path: 'commercial_invoice', message: 'the newest invoice has not been read yet' }])
    expect(e.info).toEqual({})
  })

  it('reads Retry-After', async () => {
    const res = jsonError(429, { code: 'rate_limited', message: 'slow down' }, { 'Retry-After': '3600' })
    const e = await errorFromResponse(res)
    expect(e.retryAfterSeconds).toBe(3600)
  })
})

describe('errorHint', () => {
  const err = (code: string, info: Record<string, unknown> = {}) => new ApiError(409, code, 'm', [], null, info)

  it.each([
    ['line_already_accepted', {}, 'Reject the other candidates'],
    ['document_too_large', {}, "today's model limit"],
    ['too_large', {}, 'too large to store'],
    ['not_ready', {}, 'newest file of each type'],
    ['stale_check_run', { reason: 'readings_changed' }, 'Run the checks again'],
    ['stale_check_run', { reason: 'newer_run' }, 'checks were run again'],
    ['stale_check_run', {}, 'out of date'],
    ['suggestions_stale', {}, 'earlier reading of the invoice'],
    ['suggestions_pending', { pending: 1 }, '1 candidate code from this invoice reading needs'],
    ['suggestions_pending', { pending: 3 }, '3 candidate codes from this invoice reading need'],
  ])('%s %o', (code, info, want) => {
    expect(errorHint(err(code, info))).toContain(want)
  })

  it('adds nothing to other errors', () => {
    expect(errorHint(err('internal'))).toBeNull()
  })
})
