import type { ApiError, Config } from '../api'
import { ErrorMessage } from './ErrorMessage'

export function Header({ config }: { config: Config | null }) {
  let model = 'Model: loading…'
  if (config) model = config.model_enabled ? `Model: ${config.model}` : 'Model: off'
  return (
    <header className="app-header">
      <div className="container header-inner">
        <a className="brand" href="#/">
          TradeDoc Check
        </a>
        <span className="brand-sub">Commercial invoice and packing list review</span>
        <span className="model-status">{model}</span>
      </div>
    </header>
  )
}

interface NoticeProps {
  config: Config | null
  configError: ApiError | null
  onRetry: () => void
}

// Notices stay on every page: what not to upload, and whether model output
// is real or canned.
export function Notices({ config, configError, onRetry }: NoticeProps) {
  return (
    <div className="notices">
      <p className="notice">
        <strong>Use synthetic documents only — this demo has no login.</strong> Anyone with a shipment's link can see
        it.
      </p>
      {config?.model === 'fake' && (
        <p className="notice notice-demo">
          <strong>Demo mode: canned model responses.</strong> Extraction and candidate codes return fixed sample answers
          whatever file is uploaded.
        </p>
      )}
      {config && !config.model_enabled && (
        <p className="notice notice-off">
          <strong>The model is switched off on this server.</strong> Extract and Suggest are unavailable; uploads,
          checks on existing readings, sign-off and the audit trail still work.
        </p>
      )}
      {configError && (
        <div>
          <ErrorMessage error={configError} />
          <button type="button" className="secondary" onClick={onRetry}>
            Retry loading settings
          </button>
        </div>
      )}
    </div>
  )
}
