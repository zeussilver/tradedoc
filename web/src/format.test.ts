import { describe, expect, it } from 'vitest'
import type { Config } from './api'
import {
  detailPairs,
  formatAmount,
  formatCount,
  formatDecimal,
  formatHS,
  formatKg,
  formatMoney,
  formatWait,
  minorDigits,
  type MinorUnits,
} from './format'
import { checkFile } from './upload'

// The kind of table GET /v1/config sends (currencies that are not 2 decimals).
const UNITS: MinorUnits = { JPY: 0, KRW: 0, VND: 0, CLP: 0, XOF: 0, XPF: 0, ISK: 0, BHD: 3, KWD: 3, IQD: 3, LYD: 3 }

describe('money', () => {
  it.each([
    [730200, 'USD', 'USD 7,302.00'],
    [0, 'USD', 'USD 0.00'],
    [5, 'AUD', 'AUD 0.05'],
    [500000, 'JPY', 'JPY 500,000'],
    [1234567, 'KWD', 'KWD 1,234.567'],
    // Currencies the old hard-coded browser table got wrong by 100x or 10x.
    [1500000, 'CLP', 'CLP 1,500,000'],
    [1250000, 'XPF', 'XPF 1,250,000'],
    [250000, 'XOF', 'XOF 250,000'],
    [1234567, 'IQD', 'IQD 1,234.567'],
    // The code is trimmed and upper-cased, as the server does.
    [500000, ' jpy ', 'JPY 500,000'],
    // No currency printed: no code, and the default 2 decimals.
    [192500, null, '1,925.00'],
    [192500, '', '1,925.00'],
    [-150, 'USD', 'USD -1.50'],
  ])('formatMoney(%d, %s) = %s', (minor, cur, want) => {
    expect(formatMoney(minor, cur, UNITS)).toBe(want)
  })

  it('takes minor units from the server table, default 2', () => {
    expect(minorDigits('VND', UNITS)).toBe(0)
    expect(minorDigits('BHD', UNITS)).toBe(3)
    expect(minorDigits('EUR', UNITS)).toBe(2)
    expect(minorDigits(null, UNITS)).toBe(2)
    // Before the settings load the table is empty: everything is 2.
    expect(minorDigits('JPY', {})).toBe(2)
    // A code never matches a property every object inherits.
    expect(minorDigits('constructor', UNITS)).toBe(2)
    expect(formatAmount(420000, 'USD', UNITS)).toBe('4,200.00')
    expect(formatAmount(420000, 'CLP', UNITS)).toBe('420,000')
  })
})

describe('quantities, counts and weights', () => {
  it.each([
    ['1200', '1,200'],
    ['1250.5', '1,250.5'],
    ['0.385', '0.385'], // every printed decimal kept, nothing rounded
    ['5000000', '5,000,000'],
    ['1234567.125000', '1,234,567.125000'], // trailing zeros as printed
    ['12', '12'],
    ['n/a', 'n/a'], // not a plain decimal: shown untouched
  ])('formatDecimal(%s) = %s', (s, want) => {
    expect(formatDecimal(s)).toBe(want)
  })

  it('groups whole counts', () => {
    expect(formatCount(1200)).toBe('1,200')
    expect(formatCount(84)).toBe('84')
  })

  it.each([
    [540000, '540.00 kg'],
    [936000, '936.00 kg'],
    [125, '0.125 kg'],
    [1190500, '1,190.50 kg'],
  ])('formatKg(%d) = %s', (g, want) => {
    expect(formatKg(g)).toBe(want)
  })
})

describe('formatWait', () => {
  it.each([
    [45, '45 s'],
    [60, '1 min'],
    [61, '2 min'],
    [3720, '1 h 2 min'],
    [43200, '12 h 0 min'],
  ])('formatWait(%d) = %s', (sec, want) => {
    expect(formatWait(sec)).toBe(want)
  })
})

describe('formatHS', () => {
  it.each([
    ['691110', '6911.10'],
    ['090121', '0901.21'],
    ['69111000', '6911.10.00'],
    ['abc', 'abc'],
  ])('formatHS(%s) = %s', (code, want) => {
    expect(formatHS(code)).toBe(want)
  })
})

describe('detailPairs', () => {
  it('sorts keys', () => {
    expect(detailPairs({ model: 'fake', count: '9' })).toBe('count=9 model=fake')
    expect(detailPairs(undefined)).toBe('')
  })
})

describe('checkFile', () => {
  const cfg: Config = {
    model_enabled: true,
    model: 'fake',
    max_upload_bytes: 10485760,
    max_image_bytes: 5242880,
    allowed_types: ['application/pdf', 'image/png', 'image/jpeg'],
    currency_minor_units: {},
  }
  it.each([
    [{ type: 'application/pdf', size: 3000 }, null],
    [{ type: 'application/pdf', size: 10485760 }, null],
    [{ type: 'application/pdf', size: 10485761 }, 'limit'],
    [{ type: 'image/png', size: 6000000 }, 'limit'],
    [{ type: 'application/pdf', size: 0 }, 'empty'],
    [{ type: 'application/x-msdownload', size: 100 }, 'Only PDF'],
    [{ type: '', size: 100 }, 'Only PDF'],
  ])('checkFile(%o)', (file, want) => {
    const got = checkFile(cfg, file)
    if (want === null) expect(got).toBeNull()
    else expect(got).toContain(want)
  })
})
