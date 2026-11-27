// settings-footprint.spec.ts (dashboard-15): Settings > General shows the
// Footprint panel with per-component rows (footprint-08 CHECK-09); a figure
// the API reports as null says "not measured yet", never 0.
// Payloads: e2e/mocks/settings.ts.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { footprintBody, footprintDatabase, routeFootprint } from './mocks/settings'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.removeItem('pg_sage_settings_mode'))
})

test('footprint-08 CHECK-09: General shows per-component rows against the budget',
  async ({ page }) => {
    const asked: string[] = []
    await routeFootprint(page, () => ({
      json: footprintBody([footprintDatabase('orders', true)], 120),
    }))
    page.on('request', r => {
      if (r.url().includes('/api/v1/self-cost')) asked.push(new URL(r.url()).search)
    })
    await page.goto('/#/settings')
    const panel = page.getByTestId('footprint')
    await expect(panel.getByRole('heading', { name: 'Footprint' })).toBeVisible()
    await expect(panel).toHaveAttribute('data-url', '/api/v1/self-cost')
    await expect(page.getByTestId('footprint-orders-db_time'))
      .toHaveText('Database time: 2,400 ms/h of 180,000 ms/h budget')
    await expect(page.getByTestId('footprint-orders-schema'))
      .toHaveText('Sage schema: 8.0 MiB of 10.0 GiB budget')
    await expect(page.getByTestId('footprint-orders-component-collector'))
      .toHaveText('collector 1,800 ms/h · 75% · 60 calls/h · 300 blocks/h')
    await expect(page.getByTestId('footprint-orders-component-analyzer'))
      .toHaveText('analyzer 600 ms/h · 25% · 30 calls/h · 60 blocks/h')
    await expect(panel).toContainText('(10% unattributed:')
    await expect(page.getByTestId('footprint-cpu'))
      .toHaveText('Sidecar CPU (all databases): 120 ms per collector cycle of 600 ms budget')
    // The fleet view asks for every database: no ?database= on the request.
    expect(asked).toContain('')
  })

test('null components show the not-measured state, never 0', async ({ page }) => {
  await routeFootprint(page, () => ({
    json: footprintBody([footprintDatabase('orders', false)], null),
  }))
  await page.goto('/#/settings')
  const row = page.getByTestId('footprint-orders')
  await expect(row.getByTestId('footprint-orders-db_time'))
    .toHaveText('Database time: not measured yet')
  await expect(row.getByTestId('footprint-orders-blocks'))
    .toHaveText('Shared blocks: not measured yet')
  await expect(row.getByTestId('footprint-orders-schema'))
    .toHaveText('Sage schema: not measured yet')
  await expect(row).toContainText('No component breakdown yet.')
  await expect(row).not.toContainText(/\b0 (ms|blocks|calls)/)
  await expect(page.getByTestId('footprint-cpu'))
    .toHaveText('Sidecar CPU (all databases): not measured yet')
})

test('a fleet lists each database; no databases says so', async ({ page }) => {
  let databases: unknown[] = [footprintDatabase('orders', true),
    footprintDatabase('reporting', false)]
  await page.clock.install()
  await routeFootprint(page, () => ({ json: footprintBody(databases, 120) }))
  await page.goto('/#/settings')
  await expect(page.getByTestId('footprint-orders-component-collector')).toBeVisible()
  await expect(page.getByTestId('footprint-reporting-db_time'))
    .toHaveText('Database time: not measured yet')

  databases = []
  // Footprint polls every 60 s (useAPI interval).
  await page.clock.fastForward(60_000)
  await expect(page.getByTestId('footprint')).toContainText('No databases to show.')
  await expect(page.getByTestId('footprint-orders')).toHaveCount(0)
})

test('a failed footprint read shows the error, not an empty panel', async ({ page }) => {
  await routeFootprint(page, () => ({ status: 503, json: { error: 'metadata unavailable' } }))
  await page.goto('/#/settings')
  await expect(page.getByRole('alert').filter({ hasText: 'Footprint:' }))
    .toContainText('Footprint: metadata unavailable (503)')
  await expect(page.getByTestId('footprint')).toHaveCount(0)
})
