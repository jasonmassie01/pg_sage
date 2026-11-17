// recoverability.spec.ts (drill-01d): the database page's Recoverability
// panel and the fleet overview's restore drill count. Payloads are the Go
// routes' golden responses (e2e/mocks/recoverability.ts). No concurrency
// tests: one person reads one page; the API test runs concurrent requests.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { fleetRecoverability, restoreDrills } from './mocks/recoverability'

async function routeHistory(page: Page, status: number, body: unknown) {
  const seen: string[] = []
  await page.route('**/api/v1/databases/primary/restore-drills**', route => {
    seen.push(new URL(route.request().url()).search)
    return route.fulfill({ status, json: body })
  })
  return seen
}

async function openDatabase(page: Page) {
  await page.goto('/#/database')
  await page.getByRole('combobox', { name: 'Database scope' }).selectOption('primary')
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('shows the last drill with RPO and RTO in plain units', async ({ page }) => {
  const seen = await routeHistory(page, 200, restoreDrills({ database: 'primary' }))
  await openDatabase(page)
  const panel = page.getByTestId('recoverability')
  await expect(panel).toBeVisible()
  await expect(panel.getByTestId('recoverability-status')).toHaveText('Passed')
  await expect(panel.getByTestId('recoverability-rpo')).toContainText('RPO 4 min')
  await expect(panel.getByTestId('recoverability-rto')).toContainText('RTO 11 min')
  await expect(panel.getByTestId('recoverability-cost'))
    .toContainText('about $1.40 (estimate)')
  await expect(panel.getByTestId('recoverability-failure')).toHaveCount(0)
  expect(seen).toContain('?limit=20')
  // The page's existing sections still render around the panel.
  await expect(page.getByTestId('evidence-packs')).toBeVisible()
  await expect(page.getByTestId('raw-snapshot')).toBeVisible()
})

test('a failed newest drill shows the failure banner', async ({ page }) => {
  const latest = { ...restoreDrills().drills[1] }
  await routeHistory(page, 200, restoreDrills({ database: 'primary', status: 'failed',
    reason: latest.failure, latest, last_pass: undefined, drills: [latest] }))
  await openDatabase(page)
  await expect(page.getByTestId('recoverability-failure'))
    .toContainText('row_count app.orders: 90 rows, source 100')
  await expect(page.getByTestId('recoverability-status')).toHaveText('Failed')
})

test('an API 500 shows the error, not an empty panel', async ({ page }) => {
  await routeHistory(page, 500, { error: 'restore drills of primary could not be read' })
  await openDatabase(page)
  const panel = page.getByTestId('recoverability')
  await expect(panel.getByRole('alert'))
    .toContainText('restore drills of primary could not be read')
  await expect(panel.getByTestId('recoverability-status')).toHaveCount(0)
})

test('the fleet overview reads the 35-day restore drill count', async ({ page }) => {
  await page.route('**/api/v1/fleet/recoverability**', route =>
    route.fulfill({ json: fleetRecoverability() }))
  await page.goto('/#/')
  await expect(page.getByTestId('fleet-recoverability'))
    .toContainText('2 of 6 databases passed a restore drill in the last 35 days')
})
