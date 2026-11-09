// vitest.config.js — Vitest configuration for component unit tests.
//
// Plan reference: reviews/archive/docs-internal/plan_v0.8.5.md §7.5 CHECK-T18/T19.
// Runs under jsdom so Radix Tooltip's portal/DOM APIs work; the
// `test` subtree is segregated from vite.config.js so the build
// pipeline ignores it.

import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react({ jsxRuntime: 'automatic' })],
  esbuild: {
    jsx: 'automatic',
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./vitest.setup.js'],
    include: ['src/**/*.test.{js,jsx}'],
    coverage: {
      provider: 'v8',
      reporter: ['text', 'json-summary'],
      include: ['src/components/ConfigTooltip.jsx'],
      thresholds: {
        // CHECK-T18: ConfigTooltip must hit 100% on lines it owns.
        lines: 100,
        functions: 100,
      },
    },
  },
})
