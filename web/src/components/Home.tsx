import { useId, useState, type FormEvent } from 'react'
import { api, type Config, type DocType } from '../api'
import { openShipment, useAction } from '../hooks'
import { fetchSample, uploadFile } from '../upload'
import { ErrorMessage } from './ErrorMessage'

// The synthetic samples in web/public/samples. All parties are fictional.
const SAMPLES = {
  a: {
    reference: 'Sample A - INV-2026-0042',
    title: 'Load sample A (with problems)',
    about: 'Porcelain plates, mugs and bamboo boards from Ningbo. The documents disagree in a few deliberate places.',
  },
  b: {
    reference: 'Sample B - INV-2026-0057',
    title: 'Load sample B (consistent)',
    about: 'Cotton towels, fleece throws and face cloths from Ho Chi Minh City. The documents agree.',
  },
} as const
type SampleKey = keyof typeof SAMPLES

const SAMPLE_FILES: [DocType, string][] = [
  ['commercial_invoice', 'invoice'],
  ['packing_list', 'packing-list'],
]

export function Home({ config }: { config: Config | null }) {
  const create = useAction()
  const sample = useAction()
  const [reference, setReference] = useState('')
  const [step, setStep] = useState('')
  const [partialId, setPartialId] = useState<string | null>(null)
  const refId = useId()
  // One flag for both actions: each one ends by opening its own shipment,
  // so starting the other meanwhile would open the wrong one when the first
  // finishes (a sample load takes a few seconds on AWS).
  const busy = create.busy || sample.busy

  async function onCreate(e: FormEvent) {
    e.preventDefault()
    await create.run(async () => {
      const sh = await api.createShipment(reference.trim())
      openShipment(sh.id)
    })
  }

  // A sample is loaded through the same create + upload flow a user's own
  // files go through, so it exercises the real upload path end to end.
  async function onSample(key: SampleKey) {
    if (!config) return
    setPartialId(null)
    await sample.run(async () => {
      setStep('Creating shipment…')
      const sh = await api.createShipment(SAMPLES[key].reference)
      setPartialId(sh.id)
      for (const [type, suffix] of SAMPLE_FILES) {
        const name = `sample-${key}-${suffix}.pdf`
        setStep(`Uploading ${name}…`)
        await uploadFile(config, sh.id, type, await fetchSample(name))
      }
      openShipment(sh.id)
    })
    setStep('')
  }

  return (
    <>
      <div className="home-intro">
        <h1>Check a commercial invoice against its packing list</h1>
        <p>
          Upload both documents, let the model read their fields with page-and-quote evidence, compare them line by
          line, and get candidate tariff codes for a licensed customs broker to review. A named person signs off, and
          every step is written to an audit trail.
        </p>
      </div>

      <div className="home-grid">
        <section className="panel" aria-labelledby="new-h">
          <h2 id="new-h">New shipment</h2>
          <form onSubmit={onCreate}>
            <div className="form-row">
              <label htmlFor={refId}>Your reference</label>
              <input
                id={refId}
                type="text"
                required
                maxLength={64}
                placeholder="e.g. a job number"
                value={reference}
                disabled={busy}
                onChange={(e) => setReference(e.target.value)}
              />
            </div>
            <div className="actions">
              <button type="submit" disabled={!reference.trim() || busy}>
                {create.busy ? 'Creating…' : 'Create shipment'}
              </button>
            </div>
            <ErrorMessage error={create.error} />
          </form>
          <p className="hint">
            There is no list of shipments: each one is reachable only through its link, so bookmark it once it opens.
          </p>
        </section>

        <section className="panel" aria-labelledby="samples-h">
          <h2 id="samples-h">Try a synthetic sample</h2>
          <ul className="samples">
            {(Object.keys(SAMPLES) as SampleKey[]).map((key) => (
              <li key={key}>
                <button
                  type="button"
                  className="secondary"
                  onClick={() => onSample(key)}
                  disabled={!config || busy}
                >
                  {SAMPLES[key].title}
                </button>
                <p className="hint">{SAMPLES[key].about}</p>
              </li>
            ))}
          </ul>
          {/* Always on the page, so a screen reader announces each step. */}
          <p className="busy step-status" role="status">
            {sample.busy ? step : ''}
          </p>
          <ErrorMessage error={sample.error} />
          {sample.error && partialId && (
            <p className="hint">
              The shipment was created but not every file was uploaded. <a href={`#/shipments/${partialId}`}>Open it</a>{' '}
              to upload the rest by hand.
            </p>
          )}
        </section>
      </div>
    </>
  )
}
