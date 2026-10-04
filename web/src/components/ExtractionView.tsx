import type { ChargeKind, Extraction, InvoiceExtraction, PackingListExtraction } from '../api'
import {
  formatAmount,
  formatCount,
  formatDecimal,
  formatKg,
  formatMoney,
  formatTime,
  plural,
  type MinorUnits,
} from '../format'
import { FieldValue } from './FieldValue'

const chargeLabel: Record<ChargeKind, string> = {
  freight: 'Freight',
  insurance: 'Insurance',
  discount: 'Discount (subtracted)',
  other: 'Other',
}

interface Props {
  extraction: Extraction
  total: number // how many readings the document has
  minorUnits: MinorUnits // from the server's settings
}

// ExtractionView shows one reading of a document: header fields, then the
// line items. Every value carries its confidence and evidence. The reading's
// ID is shown because check runs and candidate codes name the reading they
// were made from.
export function ExtractionView({ extraction, total, minorUnits }: Props) {
  return (
    <div className="extraction">
      <p className="run-info">
        Reading <code className="id">{extraction.id}</code>
        {total > 1 && ` (newest of ${total})`} · {runSummary(extraction)} · {formatTime(extraction.created_at)}
      </p>
      {extraction.invoice && <InvoiceView inv={extraction.invoice} units={minorUnits} />}
      {extraction.packing_list && <PackingListView pl={extraction.packing_list} />}
    </div>
  )
}

function runSummary(e: Extraction): string {
  if (!e.run) return e.source === 'manual' ? 'entered manually' : e.source
  const r = e.run
  const parts = [`model ${r.model}`, plural(r.attempts, 'attempt')]
  // The fake model reports no token usage, so zero counts are left out.
  if (r.input_tokens > 0 || r.output_tokens > 0) {
    parts.push(`${formatCount(r.input_tokens)} input / ${formatCount(r.output_tokens)} output tokens`)
  }
  return parts.join(' · ')
}

function InvoiceView({ inv, units }: { inv: InvoiceExtraction; units: MinorUnits }) {
  // Amounts are formatted with the currency read from the same invoice. If
  // none was printed (a bare "$" is not a currency), the Currency field says
  // "not found in document" and amounts are shown with 2 decimals and no
  // code, as the checks do, rather than with a guessed one.
  const cur = inv.currency.value
  const curLabel = cur ? ` (${cur.trim().toUpperCase()})` : ''
  const charges = inv.charges ?? []
  return (
    <>
      <dl className="fields">
        <div>
          <dt>Invoice number</dt>
          <dd>
            <FieldValue field={inv.invoice_number} />
          </dd>
        </div>
        <div>
          <dt>Invoice date</dt>
          <dd>
            <FieldValue field={inv.invoice_date} />
          </dd>
        </div>
        <div>
          <dt>Seller</dt>
          <dd>
            <FieldValue field={inv.seller_name} />
          </dd>
        </div>
        <div>
          <dt>Buyer</dt>
          <dd>
            <FieldValue field={inv.buyer_name} />
          </dd>
        </div>
        <div>
          <dt>Currency</dt>
          <dd>
            <FieldValue field={inv.currency} />
          </dd>
        </div>
        <div>
          <dt>Incoterm</dt>
          <dd>
            <FieldValue field={inv.incoterm} />
          </dd>
        </div>
        <div>
          <dt>Invoice total</dt>
          <dd>
            <FieldValue field={inv.total_minor} format={(v) => formatMoney(v, cur, units)} />
          </dd>
        </div>
      </dl>
      <div className="table-wrap">
        <table className="data">
          <caption className="sr-only">Invoice line items</caption>
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">Description</th>
              <th scope="col" className="num">
                Qty
              </th>
              <th scope="col">Unit</th>
              <th scope="col" className="num">
                Unit price{curLabel}
              </th>
              <th scope="col" className="num">
                Line total{curLabel}
              </th>
              <th scope="col">Origin</th>
            </tr>
          </thead>
          <tbody>
            {(inv.line_items ?? []).map((l) => (
              <tr key={l.line_no}>
                <td className="num">{l.line_no}</td>
                <td className="desc">
                  <FieldValue field={l.description} />
                </td>
                {/* Quantities and unit prices keep every printed decimal, e.g. "0.385". */}
                <td className="num">
                  <FieldValue field={l.quantity} format={formatDecimal} />
                </td>
                <td>
                  <FieldValue field={l.unit} />
                </td>
                <td className="num">
                  <FieldValue field={l.unit_price} format={formatDecimal} />
                </td>
                <td className="num">
                  <FieldValue field={l.line_total_minor} format={(v) => formatAmount(v, cur, units)} />
                </td>
                <td>
                  <FieldValue field={l.origin_country} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {charges.length > 0 ? (
        <div className="table-wrap">
          <table className="data">
            <caption className="sr-only">Charges printed outside the goods lines</caption>
            <thead>
              <tr>
                <th scope="col">Charge</th>
                <th scope="col">As printed</th>
                <th scope="col" className="num">
                  Amount{curLabel}
                </th>
              </tr>
            </thead>
            <tbody>
              {charges.map((c, i) => (
                <tr key={i}>
                  <td className="nowrap">{chargeLabel[c.kind] ?? c.kind}</td>
                  <td className="desc">
                    <FieldValue field={c.label} />
                  </td>
                  <td className="num">
                    <FieldValue field={c.amount_minor} format={(v) => formatAmount(v, cur, units)} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        // Worth saying: Australia values imports on an FOB basis, so a broker
        // looks for freight and insurance rows on the invoice.
        <p className="hint">No freight, insurance, discount or other charge rows were read from this invoice.</p>
      )}
    </>
  )
}

function PackingListView({ pl }: { pl: PackingListExtraction }) {
  return (
    <>
      <dl className="fields">
        <div>
          <dt>Packing list number</dt>
          <dd>
            <FieldValue field={pl.packing_list_number} />
          </dd>
        </div>
        <div>
          <dt>Invoice reference</dt>
          <dd>
            <FieldValue field={pl.invoice_ref} />
          </dd>
        </div>
        <div>
          <dt>Total packages</dt>
          <dd>
            <FieldValue field={pl.total_packages} format={formatCount} />
          </dd>
        </div>
        <div>
          <dt>Total gross weight</dt>
          <dd>
            <FieldValue field={pl.total_gross_weight_grams} format={formatKg} />
          </dd>
        </div>
      </dl>
      <div className="table-wrap">
        <table className="data">
          <caption className="sr-only">Packing list line items</caption>
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">Description</th>
              <th scope="col" className="num">
                Qty
              </th>
              <th scope="col">Unit</th>
              <th scope="col" className="num">
                Packages
              </th>
              <th scope="col" className="num">
                Gross weight
              </th>
            </tr>
          </thead>
          <tbody>
            {(pl.line_items ?? []).map((l) => (
              <tr key={l.line_no}>
                <td className="num">{l.line_no}</td>
                <td className="desc">
                  <FieldValue field={l.description} />
                </td>
                <td className="num">
                  <FieldValue field={l.quantity} format={formatDecimal} />
                </td>
                <td>
                  <FieldValue field={l.unit} />
                </td>
                <td className="num">
                  <FieldValue field={l.packages} format={formatCount} />
                </td>
                <td className="num">
                  <FieldValue field={l.gross_weight_grams} format={formatKg} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  )
}
