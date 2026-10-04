// Pure display helpers. Money and weights arrive as integers (minor units,
// grams), so they are formatted with integer maths and string padding,
// never by dividing into a float: 0.1 + 0.2 !== 0.3. Quantities and unit
// prices arrive as decimal strings and stay strings.

import type { DocType } from './api'

// MinorUnits is the server's table of currencies whose minor unit is not 2
// decimals (ISO 4217), from GET /v1/config, e.g. {"JPY": 0, "KWD": 3}. The
// checks format their findings with the same table, so an amount reads the
// same in the extracted fields and in a finding. A currency that is not in
// the table has 2 decimals, the ISO 4217 default.
export type MinorUnits = Record<string, number>

// normCurrency trims and upper-cases a currency code the way the server does,
// so " usd" and "USD" are the same currency. null or blank gives "".
function normCurrency(currency: string | null): string {
  return (currency ?? '').trim().toUpperCase()
}

export function minorDigits(currency: string | null, units: MinorUnits): number {
  const code = normCurrency(currency)
  // hasOwn, so a code can never pick up a property every object inherits.
  const digits = code && Object.hasOwn(units, code) ? units[code] : undefined
  return digits ?? 2
}

// groupThousands("1234567") === "1,234,567"
export function groupThousands(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',')
}

// splitDecimal(123456, 2) === "1,234.56"
function splitDecimal(n: number, decimals: number): string {
  const sign = n < 0 ? '-' : ''
  const s = String(Math.abs(Math.trunc(n))).padStart(decimals + 1, '0')
  const whole = groupThousands(s.slice(0, s.length - decimals))
  const frac = decimals > 0 ? '.' + s.slice(s.length - decimals) : ''
  return sign + whole + frac
}

// formatAmount(730200, "USD", {}) === "7,302.00";
// formatAmount(5000, "JPY", {JPY: 0}) === "5,000"
export function formatAmount(minor: number, currency: string | null, units: MinorUnits): string {
  return splitDecimal(minor, minorDigits(currency, units))
}

// formatMoney(730200, "USD", {}) === "USD 7,302.00". Without a currency
// (the invoice printed only "$", say) the amount is shown with no code
// rather than a guessed one.
export function formatMoney(minor: number, currency: string | null, units: MinorUnits): string {
  const amount = formatAmount(minor, currency, units)
  const code = normCurrency(currency)
  return code ? `${code} ${amount}` : amount
}

// formatCount(1200) === "1,200", for whole numbers such as packages.
export function formatCount(n: number): string {
  return splitDecimal(n, 0)
}

// formatDecimal("1250.5") === "1,250.5". Quantities and unit prices arrive
// as the decimal string printed on the document; only thousands separators
// are added, for reading. The string is never turned into a float, so
// "0.385" stays "0.385" and nothing is rounded or padded. Anything that is
// not a plain decimal is shown untouched.
export function formatDecimal(s: string): string {
  const m = /^(\d+)(\.\d+)?$/.exec(s)
  if (!m) return s
  const [, whole = '', frac = ''] = m
  return groupThousands(whole) + frac
}

// formatKg(540000) === "540.00 kg"; formatKg(125) === "0.125 kg".
// Packing lists usually print 2 decimals, so 3 are shown only when needed.
export function formatKg(grams: number): string {
  let s = splitDecimal(grams, 3)
  if (s.endsWith('0')) s = s.slice(0, -1)
  return `${s} kg`
}

// formatHS("691110") === "6911.10"; formatHS("69111000") === "6911.10.00".
// HS codes are strings: leading zeros matter ("0901.21").
export function formatHS(code: string): string {
  if (/^\d{6}$/.test(code)) return `${code.slice(0, 4)}.${code.slice(4)}`
  if (/^\d{8}$/.test(code)) return `${code.slice(0, 4)}.${code.slice(4, 6)}.${code.slice(6)}`
  return code
}

// formatWait(45) === "45 s"; formatWait(3720) === "1 h 2 min". A server's
// Retry-After can be hours away: the daily model limit resets at midnight UTC.
export function formatWait(seconds: number): string {
  if (seconds < 60) return `${seconds} s`
  const minutes = Math.ceil(seconds / 60)
  if (minutes < 60) return `${minutes} min`
  return `${Math.floor(minutes / 60)} h ${minutes % 60} min`
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

// The first 12 hex characters are plenty to tell two files apart on screen.
export function shortHash(sha256: string): string {
  return sha256.slice(0, 12)
}

const pad2 = (n: number) => String(n).padStart(2, '0')

// formatTime gives "2026-10-04 14:03:22" in the viewer's local time zone:
// unambiguous for an audit trail, unlike "04/10/2026".
export function formatTime(iso: string | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return (
    `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ` +
    `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
  )
}

export function docTypeLabel(t: DocType | string): string {
  switch (t) {
    case 'commercial_invoice':
      return 'Commercial invoice'
    case 'packing_list':
      return 'Packing list'
    default:
      return t
  }
}

// plural(1, "error") === "1 error"; plural(2, "error") === "2 errors"
export function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

// detailPairs({b: "2", a: "1"}) === "a=1 b=2", sorted so rows line up.
export function detailPairs(detail: Record<string, string> | undefined): string {
  if (!detail) return ''
  return Object.keys(detail)
    .sort()
    .map((k) => `${k}=${detail[k]}`)
    .join(' ')
}
