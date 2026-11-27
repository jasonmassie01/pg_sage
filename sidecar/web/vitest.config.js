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
    include: ['src/**/*.test.{js,jsx}', 'scripts/**/*.test.mjs'],
    // CI runs `npm run test -- --coverage`, so these floors fail the build.
    // The shared data layer (hooks, lib) is held at 70% on its own glob,
    // so a well-covered lib cannot hide an untested hook.
    coverage: {
      provider: 'v8',
      reporter: ['text', 'json-summary'],
      include: ['src/hooks/**', 'src/lib/**', 'src/components/ConfigTooltip.jsx'],
      thresholds: {
        'src/hooks/**': { lines: 70, functions: 70 },
        'src/lib/**': { lines: 70, functions: 70 },
        // CHECK-T18: ConfigTooltip must hit 100% on lines it owns.
        'src/components/ConfigTooltip.jsx': { lines: 100, functions: 100 },
      },
    },
  },
})
