import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'

// Build into ../cmd/server/web_dist so `go build` embeds the latest UI
// without an extra copy step. Dev server proxies /api to the Go server.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: path.resolve(__dirname, '../cmd/server/web_dist'),
    emptyOutDir: true,
  },
  server: {
    port: 5174,
    proxy: {
      '/api': 'http://localhost:8787',
    },
  },
})
