import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  api,
  asApiError,
  type ApiError,
  type AuditEvent,
  type CheckRun,
  type Config,
  type DocType,
  type DocumentMeta,
  type Extraction,
  type Shipment,
  type SignOff,
  type Suggestion,
} from '../api'
import { formatTime } from '../format'
import { currentReadings, isRead, newestOfType, runIsCurrent } from '../readings'
import { pendingFor } from '../suggestions'
import { AuditPanel } from './AuditPanel'
import { ChecksPanel } from './ChecksPanel'
import { DocumentCard } from './DocumentCard'
import { ErrorMessage } from './ErrorMessage'
import { SignOffPanel } from './SignOffPanel'
import { SuggestionsPanel } from './SuggestionsPanel'
import { PanelTitle } from './PanelTitle'

const DOC_TYPES: DocType[] = ['commercial_invoice', 'packing_list']

interface Props {
  shipmentId: string
  config: Config | null
  reviewer: string
  onReviewerChange: (name: string) => void
}

// Workspace is one shipment's page. It owns the data; each panel gets what
// it shows plus an onChanged callback that reloads the parts it affected.
export function Workspace({ shipmentId, config, reviewer, onReviewerChange }: Props) {
  const [shipment, setShipment] = useState<Shipment | null>(null)
  const [notFound, setNotFound] = useState(false)
  // null until the documents have loaded, so nothing is called out of date
  // just because the check runs happened to arrive first.
  const [docs, setDocs] = useState<DocumentMeta[] | null>(null)
  const [extractions, setExtractions] = useState<Record<string, Extraction[]>>({})
  const [runs, setRuns] = useState<CheckRun[]>([])
  const [suggestions, setSuggestions] = useState<Suggestion[]>([])
  const [signOffs, setSignOffs] = useState<SignOff[]>([])
  const [events, setEvents] = useState<AuditEvent[]>([])
  const [loadError, setLoadError] = useState<ApiError | null>(null)

  // One loader per part of the page, so an action reloads only what it
  // changed (the API is rate limited per client). Each loader gets a
  // "latest" check and only stores its answer if no newer request for the
  // same part has started since: two quick clicks fire two reloads, and if
  // the first answer arrives last it must not overwrite the second.
  const loaders = useMemo(
    () => ({
      documents: async (latest: () => boolean) => {
        const list = await api.listDocuments(shipmentId)
        // Only the documents shown in the cards need their readings.
        const shown = DOC_TYPES.map((t) => newestOfType(list, t)).filter(
          (d): d is DocumentMeta => d?.status === 'extracted',
        )
        const pairs = await Promise.all(shown.map(async (d) => [d.id, await api.listExtractions(d.id)] as const))
        // Checked after both steps, and both are set together, so the
        // documents and their readings always come from the same request.
        if (!latest()) return
        setDocs(list)
        setExtractions(Object.fromEntries(pairs))
      },
      checks: async (latest: () => boolean) => {
        const list = await api.listCheckRuns(shipmentId)
        if (latest()) setRuns(list)
      },
      suggestions: async (latest: () => boolean) => {
        const list = await api.listSuggestions(shipmentId)
        if (latest()) setSuggestions(list)
      },
      signOffs: async (latest: () => boolean) => {
        const list = await api.listSignOffs(shipmentId)
        if (latest()) setSignOffs(list)
      },
      audit: async (latest: () => boolean) => {
        const list = await api.listAudit(shipmentId)
        if (latest()) setEvents(list)
      },
    }),
    [shipmentId],
  )
  type Part = keyof typeof loaders

  // generation counts the requests started for each part; a loader's answer
  // is the latest only while the count is still the one it started with.
  const generation = useRef<Partial<Record<Part, number>>>({})

  // refresh reloads the named parts. The audit trail is always included
  // because every action writes to it.
  const refresh = useCallback(
    async (...parts: Part[]) => {
      try {
        await Promise.all(
          [...parts, 'audit' as const].map((p) => {
            const n = (generation.current[p] ?? 0) + 1
            generation.current[p] = n
            return loaders[p](() => generation.current[p] === n)
          }),
        )
        setLoadError(null)
      } catch (e) {
        setLoadError(asApiError(e))
      }
    },
    [loaders],
  )
  const refreshAll = useCallback(() => refresh('documents', 'checks', 'suggestions', 'signOffs'), [refresh])

  useEffect(() => {
    let cancelled = false
    api.getShipment(shipmentId).then(
      (sh) => {
        if (cancelled) return
        setShipment(sh)
        document.title = `${sh.reference} · TradeDoc Check`
        void refreshAll()
      },
      (e) => {
        if (cancelled) return
        const err = asApiError(e)
        if (err.status === 404) setNotFound(true)
        else setLoadError(err)
      },
    )
    return () => {
      cancelled = true
    }
  }, [shipmentId, refreshAll])

  if (notFound) {
    return (
      <section className="panel">
        <h1>Shipment not found</h1>
        <p>
          No shipment has the ID <code className="id">{shipmentId}</code>. Check the link, or{' '}
          <a href="#/">start a new shipment</a>.
        </p>
      </section>
    )
  }
  if (!shipment) {
    return loadError ? <ErrorMessage error={loadError} /> : <p className="muted">Loading shipment…</p>
  }

  const docList = docs ?? []
  const docTypes = new Map(docList.map((d) => [d.id, d.type] as const))
  // Which readings are current (the newest reading of the newest document of
  // each type) and whether the newest check run compared exactly those. The
  // server applies the same rules, so these only explain in advance what it
  // would refuse.
  const current = docs ? currentReadings(docs, extractions) : null
  const latestRun = runs.at(-1)
  const runCurrent = !latestRun || !current || runIsCurrent(latestRun, current)
  // Approval counts candidate codes made from the invoice reading the run
  // checked; codes from an older reading no longer stand behind it.
  const runInvoice = latestRun?.invoice_extraction_id
  const pending = pendingFor(suggestions, runInvoice)
  const suggestionsStale = suggestions.length > 0 && !suggestions.some((s) => s.extraction_id === runInvoice)

  return (
    <>
      <div className="shipment-head">
        <p className="eyebrow">Shipment</p>
        <h1>{shipment.reference}</h1>
        <p className="run-info">
          <code className="id">{shipment.id}</code> · created {formatTime(shipment.created_at)} · Bookmark this page:
          there is no shipment list, so the link is the only way back.
        </p>
      </div>

      {loadError && (
        <div>
          <ErrorMessage error={loadError} />
          <button type="button" className="secondary" onClick={refreshAll}>
            Reload
          </button>
        </div>
      )}

      <section className="panel" aria-labelledby="docs-h">
        <PanelTitle id="docs-h" step={1}>
          Documents
        </PanelTitle>
        <p className="panel-intro">
          Upload the invoice and the packing list, then extract their fields. Each value shows the model's own
          confidence (high / medium / low) and the page and exact text it was read from.
        </p>
        <div className="doc-list">
          {DOC_TYPES.map((t) => {
            const doc = newestOfType(docList, t)
            return (
              <DocumentCard
                key={t}
                type={t}
                shipmentId={shipment.id}
                config={config}
                doc={doc}
                extractions={(doc && extractions[doc.id]) ?? []}
                onChanged={() => void refresh('documents')}
              />
            )
          })}
        </div>
      </section>

      <ChecksPanel
        shipmentId={shipment.id}
        runs={runs}
        docTypes={docTypes}
        ready={isRead(docList, 'commercial_invoice') && isRead(docList, 'packing_list')}
        current={current}
        runCurrent={runCurrent}
        onChanged={() => void refresh('checks')}
      />

      <SuggestionsPanel
        shipmentId={shipment.id}
        config={config}
        suggestions={suggestions}
        ready={isRead(docList, 'commercial_invoice')}
        current={current}
        reviewer={reviewer}
        onReviewerChange={onReviewerChange}
        onChanged={() => void refresh('suggestions')}
      />

      <SignOffPanel
        shipmentId={shipment.id}
        latestRun={latestRun}
        runCurrent={runCurrent}
        signOffs={signOffs}
        pendingSuggestions={pending}
        suggestionsStale={suggestionsStale}
        reviewer={reviewer}
        onReviewerChange={onReviewerChange}
        // Everything a 409 can be about: a newer run, documents read again
        // (stale_check_run), or candidate codes still open or out of date.
        onChanged={() => void refresh('signOffs', 'checks', 'documents', 'suggestions')}
      />

      <AuditPanel events={events} />
    </>
  )
}
