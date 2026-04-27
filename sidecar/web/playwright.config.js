// playwright.config.js — e2e test config for the dashboard.
//
// Plan reference: docs/plan_v0.8.5.md §7.5 CHECK-T20 (tooltip e2e).
// Runs against the Vite dev server. CI is expected to
// `npm run build` + serve dist/, but for local iteration this config
// starts the dev server on demand.
//
// Override the port via PLAYWRIGHT_PORT env var if the default
// conflicts with another service (e.g. Docker).

/* global process */
import { defineConfig, devices } from '@playwright/test'

const port = process.env.PLAYWRIGHT_PORT || '5175'
const baseURL = `http://localhost:${port}`

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  fullyParallel: true,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL,
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  // Spin up Vite on demand so `npm run test:e2e` works standalone.
  webServer: {
    command: `npx vite --port ${port} --strictPort`,
    url: baseURL,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
})
