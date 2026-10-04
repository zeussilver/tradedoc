import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, asApiError } from './api'

// useAction wraps one button's async work: it tracks "busy" (to disable the
// button) and keeps the last error to show next to that button.
export function useAction() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)
  // A ref, not state, so a fast double click is ignored even before React
  // has re-rendered the button as disabled.
  const running = useRef(false)

  const run = useCallback(async (work: () => Promise<unknown>): Promise<boolean> => {
    if (running.current) return false
    running.current = true
    setBusy(true)
    setError(null)
    try {
      await work()
      return true
    } catch (e) {
      setError(asApiError(e))
      return false
    } finally {
      running.current = false
      setBusy(false)
    }
  }, [])

  return { busy, error, run, setError }
}

// useElapsed counts whole seconds while `running` is true, for model calls
// that take 30-90 seconds: a ticking number shows the page hasn't frozen.
export function useElapsed(running: boolean): number {
  const [seconds, setSeconds] = useState(0)
  useEffect(() => {
    if (!running) return
    const start = Date.now()
    setSeconds(0)
    const timer = setInterval(() => setSeconds(Math.floor((Date.now() - start) / 1000)), 1000)
    return () => clearInterval(timer)
  }, [running])
  return seconds
}

// Routing is by URL hash (#/shipments/<id>) so a refresh or a shared link
// reopens the same shipment, the static server needs no rewrite rules, and
// the fragment is never sent to the server or written to its logs.
const SHIPMENT_ROUTE = /^#\/shipments\/([A-Za-z0-9_-]{1,100})$/

function shipmentIdFromHash(): string | null {
  return SHIPMENT_ROUTE.exec(window.location.hash)?.[1] ?? null
}

export function useShipmentRoute(): string | null {
  const [id, setId] = useState(shipmentIdFromHash)
  useEffect(() => {
    const onChange = () => setId(shipmentIdFromHash())
    window.addEventListener('hashchange', onChange)
    return () => window.removeEventListener('hashchange', onChange)
  }, [])
  return id
}

export function openShipment(id: string) {
  window.location.hash = `#/shipments/${id}`
}

// The reviewer's name is remembered in this browser only, as a convenience.
// Storage can be blocked (private mode, strict settings), so every access is
// wrapped: the page still works, the name just isn't remembered.
const REVIEWER_KEY = 'tradedoc.reviewer'

function readReviewer(): string {
  try {
    return window.localStorage.getItem(REVIEWER_KEY) ?? ''
  } catch {
    return ''
  }
}

export function useReviewerName(): [string, (name: string) => void] {
  const [name, setName] = useState(readReviewer)
  const update = useCallback((next: string) => {
    setName(next)
    try {
      window.localStorage.setItem(REVIEWER_KEY, next)
    } catch {
      // Storage unavailable: keep the name for this page view only.
    }
  }, [])
  return [name, update]
}
