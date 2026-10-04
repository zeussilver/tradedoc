import { useId, useState, type ChangeEvent } from 'react'
import { api, type Config, type DocStatus, type DocType, type DocumentMeta, type Extraction } from '../api'
import { docTypeLabel, formatBytes, formatTime, shortHash } from '../format'
import { useAction } from '../hooks'
import { uploadFile } from '../upload'
import { Badge, type Tone } from './Badge'
import { Elapsed } from './Elapsed'
import { ErrorMessage } from './ErrorMessage'
import { ExtractionView } from './ExtractionView'

const statusLabel: Record<DocStatus, string> = {
  pending_upload: 'upload pending',
  uploaded: 'uploaded',
  extracted: 'extracted',
  rejected: 'rejected',
}
const statusTone: Record<DocStatus, Tone> = {
  pending_upload: 'warning',
  uploaded: 'info',
  extracted: 'ok',
  rejected: 'error',
}

interface Props {
  type: DocType
  shipmentId: string
  config: Config | null
  doc?: DocumentMeta // the newest document of this type, if any
  extractions: Extraction[] // all readings of that document, oldest first
  onChanged: () => void
}

// DocumentCard is one slot of the shipment (invoice or packing list): the
// file, its upload status, the Extract button and the newest reading.
export function DocumentCard({ type, shipmentId, config, doc, extractions, onChanged }: Props) {
  const upload = useAction()
  const extract = useAction()
  const [step, setStep] = useState('')
  const inputId = useId()
  const label = docTypeLabel(type)
  const newest = extractions.at(-1)

  async function onFile(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0]
    e.target.value = '' // so choosing the same file again (after an error) fires onChange
    if (!file || !config) return
    await upload.run(() => uploadFile(config, shipmentId, type, file, setStep))
    setStep('')
    onChanged()
  }

  async function onExtract() {
    if (!doc) return
    await extract.run(() => api.extract(doc.id))
    // Refresh even after a failure: failed model calls are audited too.
    onChanged()
  }

  const modelOn = config?.model_enabled ?? false
  const extractable = doc?.status === 'uploaded' || doc?.status === 'extracted'

  return (
    <article className="card doc-card" aria-labelledby={`${inputId}-h`}>
      <header className="card-head">
        <h3 id={`${inputId}-h`}>{label}</h3>
        {doc && <Badge tone={statusTone[doc.status]}>{statusLabel[doc.status]}</Badge>}
      </header>

      {doc ? (
        <dl className="meta">
          <div>
            <dt>File</dt>
            <dd>{doc.filename}</dd>
          </div>
          <div>
            <dt>Size</dt>
            <dd>{formatBytes(doc.size_bytes)}</dd>
          </div>
          <div>
            <dt>Content type</dt>
            <dd>{doc.content_type}</dd>
          </div>
          <div>
            <dt>SHA-256</dt>
            <dd>
              {doc.sha256 ? (
                <code title={doc.sha256}>{shortHash(doc.sha256)}…</code>
              ) : (
                <span className="muted">not computed yet</span>
              )}
            </dd>
          </div>
          <div>
            <dt>Uploaded</dt>
            <dd>{formatTime(doc.uploaded_at)}</dd>
          </div>
        </dl>
      ) : (
        <p className="muted">No file uploaded yet.</p>
      )}
      {doc?.status === 'rejected' && (
        <p className="hint">
          The stored file did not match its declared size or type, so it was deleted. Upload it again.
        </p>
      )}

      <div className="actions">
        <div className="form-row file-row">
          <label htmlFor={inputId}>
            {doc ? `Upload a new ${label.toLowerCase()}` : `Upload ${label.toLowerCase()}`}
          </label>
          <input
            id={inputId}
            type="file"
            accept={config?.allowed_types.join(',')}
            disabled={!config || upload.busy}
            onChange={onFile}
          />
          <span className="hint">
            PDF, PNG or JPEG
            {config &&
              ` · up to ${formatBytes(config.max_upload_bytes)} (images ${formatBytes(config.max_image_bytes)})`}
          </span>
        </div>
      </div>
      {/* Always on the page, so a screen reader announces each step. */}
      <p className="busy step-status" role="status">
        {upload.busy ? step : ''}
      </p>
      <ErrorMessage error={upload.error} />

      <div className="actions">
        <button type="button" onClick={onExtract} disabled={!modelOn || !extractable || extract.busy}>
          {newest ? 'Extract again' : 'Extract fields'}
        </button>
        <Elapsed running={extract.busy} label="Reading the document…" hint="usually 30–90 s" />
        {!modelOn && config && <span className="hint">The model is switched off on this server.</span>}
        {modelOn && !extractable && <span className="hint">Upload the file first.</span>}
      </div>
      <ErrorMessage error={extract.error} />

      {newest && (
        <ExtractionView
          extraction={newest}
          total={extractions.length}
          // Until the settings load, every currency gets the default 2 decimals.
          minorUnits={config?.currency_minor_units ?? {}}
        />
      )}
    </article>
  )
}
