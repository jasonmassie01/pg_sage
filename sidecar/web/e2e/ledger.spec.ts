// ledger.spec.ts (ux-02a): the outcome ledger page renders one row per
// change, opens the change in Actions, and Value's incident evidence id
// links to the ledger rows that cite it. Payloads: e2e/mocks/outcomes.ts.
// No concurrency tests: the page is a read-only view one person drives.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { routeOutcomes, valueWithIncident } from './mocks/outcomes'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('renders a row with its verdict in words', async ({ page }) => {
  const seen = await routeOutcomes(page)
  await page.goto('/#/ledger')
  const row = page.getByTestId('ledger-row-primary-12')
  await expect(row).toContainText('Add an index')
  await expect(row).toContainText('Improved')
  await expect(row).toContainText('-42% blocks_per_call')
  await expect(page.getByTestId('ledger-summary')).toContainText('1 of 1 with a measured')
  expect(seen[0]).toContain('since=24h')
})

test('opens the row in Actions', async ({ page }) => {
  await routeOutcomes(page)
  await page.goto('/#/ledger')
  await page.getByRole('link', { name: /open action 12/i }).click()
  await expect(page).toHaveURL(/#\/actions\?action=12$/)
})

test("Value's evidence id is a link to the ledger", async ({ page }) => {
  const seen = await routeOutcomes(page)
  await page.route(url => url.pathname === '/api/v1/value', route =>
    route.fulfill({ json: valueWithIncident }))
  await page.goto('/#/value') // ux-01b: Value moved from home to #/value
  await page.getByRole('link', { name: /ev-xid-7/ }).click()
  await expect(page).toHaveURL(/#\/ledger\?evidence=ev-xid-7$/)
  await expect(page.getByTestId('ledger-evidence-filter')).toContainText('ev-xid-7')
  await expect.poll(() => seen.some(s => s.includes('evidence=ev-xid-7'))).toBe(true)
})
