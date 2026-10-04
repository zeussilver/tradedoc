import type { ReactNode } from 'react'

// Tones are the only colours the UI uses for meaning, so the same red means
// "needs attention" everywhere.
export type Tone = 'neutral' | 'info' | 'ok' | 'warning' | 'error'

export function Badge({ tone, title, children }: { tone: Tone; title?: string; children: ReactNode }) {
  return (
    <span className={`badge tone-${tone}`} title={title}>
      {children}
    </span>
  )
}
