import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// The console is served by the Weave binary under /admin; the dev server
// forwards API calls to a local Weave so cookies stay same-origin.
export default defineConfig({
  base: '/admin/',
  plugins: [react()],
  build: { outDir: 'dist', assetsDir: 'assets', sourcemap: false },
  // Tests also read the workflow fixture the Go validator checks.
  // Keep the browser-facing Host paired with Origin for cookie write checks.
  server: { proxy: { '/v1': { target: process.env.WEAVE_URL ?? 'http://127.0.0.1:8080', changeOrigin: false } }, fs: { allow: ['.', '../../internal/kernel/workflow/machine/testdata'] } },
  test: { environment: 'jsdom' },
})
