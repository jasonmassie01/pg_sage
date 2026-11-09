// playwright.config.js — mocked e2e suite for the dashboard.
//
// Every spec stubs /api/* through e2e/fixtures.ts, so no backend runs.
// The config starts the Vite dev server on demand, locally and in the CI
// test job (ci-08), which runs `npx playwright test` with CI=1.
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
    command: `npx vite --port ${port} --strictPort`,
    url: baseURL,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
})
