import { useCallback, useEffect, useState } from 'react'
import { api, type Config } from './api'
import { Header, Notices } from './components/Header'
import { Home } from './components/Home'
import { Workspace } from './components/Workspace'
import { useAction, useReviewerName, useShipmentRoute } from './hooks'

export function App() {
  const shipmentId = useShipmentRoute()
  const [config, setConfig] = useState<Config | null>(null)
  const configLoad = useAction()
  const [reviewer, setReviewer] = useReviewerName()

  // The server's settings (model on/off, upload limits) are read once on load.
  const { run } = configLoad
  const loadConfig = useCallback(() => void run(async () => setConfig(await api.getConfig())), [run])
  useEffect(loadConfig, [loadConfig])

  useEffect(() => {
    if (!shipmentId) document.title = 'TradeDoc Check'
  }, [shipmentId])

  return (
    <>
      <Header config={config} />
      <div className="container">
        <Notices config={config} configError={configLoad.error} onRetry={loadConfig} />
      </div>
      <main className="container">
        {shipmentId ? (
          // key: a different shipment gets a fresh workspace, not stale state.
          <Workspace
            key={shipmentId}
            shipmentId={shipmentId}
            config={config}
            reviewer={reviewer}
            onReviewerChange={setReviewer}
          />
        ) : (
          <Home config={config} />
        )}
      </main>
      <footer className="app-footer">
        <div className="container">
          TradeDoc Check is a portfolio project. Findings and candidate codes support a person's review; they do not
          replace it.
        </div>
      </footer>
    </>
  )
}
