// Removes the previous Vite build from internal/webui/dist but keeps
// .gitkeep, which Go needs so `//go:embed all:dist` works before any build.
import { readdirSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const dist = fileURLToPath(new URL('../../internal/webui/dist/', import.meta.url))

for (const name of readdirSync(dist)) {
  if (name !== '.gitkeep') {
    rmSync(join(dist, name), { recursive: true, force: true })
  }
}
