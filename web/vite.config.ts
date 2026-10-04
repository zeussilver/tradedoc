import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  build: {
    // The Go server embeds this folder (internal/webui), so one binary
    // serves both the API and the UI.
    outDir: '../internal/webui/dist',
    // Keep dist/.gitkeep: Go's //go:embed fails if the folder is empty, and a
    // fresh clone without Node must still build. scripts/clean-dist.mjs
    // removes the previous build's files instead.
    emptyOutDir: false,
  },
  server: {
    // In development the UI runs on Vite (port 5173) and the API on Go
    // (port 8080). Proxying keeps every request same-origin, as in production.
    proxy: {
      '/v1': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
    },
  },
})
