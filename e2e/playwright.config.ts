import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  // The suite needs the scripted fixture; without it the run fails, never skips.
  globalSetup: './global-setup.ts',
  timeout: 30000,
  retries: 0,
  use: {
    baseURL: process.env.PG_SAGE_E2E_BASE_URL || 'http://localhost:8085',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'chromium', use: { browserName: 'chromium' } },
  ],
  outputDir: './test-results',
});
