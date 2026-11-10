// playwright.config.js — mocked e2e suite for the dashboard.
//
// Every spec stubs /api/* through e2e/fixtures.ts, so no backend runs.
// Plan reference: docs/plan_v0.8.5.md §7.5 CHECK-T20 (tooltip e2e).
// Locally this starts the Vite dev server on demand. Under CI (the test
// job runs `npx playwright test` with CI=1, ci-08) it builds the bundle
// and serves it with `vite preview`, so the specs run against the same
// build the sidecar embeds and ships.
//
// Override the port via PLAYWRIGHT_PORT env var if the default
// conflicts with another service (e.g. Docker).

/* global process */
import { defineConfig, devices } from '@playwright/test'

const port = process.env.PLAYWRIGHT_PORT || '5175'
const baseURL = `http://localhost:${port}`
const serverCommand = process.env.CI
  ? `npx vite build && npx vite preview --port ${port} --strictPort`
  : `npx vite --port ${port} --strictPort`

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  fullyParallel: true,
  retries: process.env.CI ? 2 : 0,
  // Under CI, `list` names every spec file and test in the log next to
  // the GitHub annotations, so a run shows which specs passed or skipped.
  reporter: process.env.CI ? [['github'], ['list']] : 'list',
  use: {
    baseURL,
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? {
          launchOptions: { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE },
        } : {}),
      },
    },
  ],
  // Spin up Vite on demand so `npm run test:e2e` works standalone.
  webServer: {
    command: serverCommand,
    url: baseURL,
    reuseExistingServer: !process.env.CI,
    timeout: process.env.CI ? 180_000 : 60_000,
  },
})
