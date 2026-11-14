// value-cost.spec.ts (dashboard-15): the Value page Cost card with operator
// rates (dollars with their source and date) and without them ("Rates not
// configured", no figure) (market-04 CHECK-04). pg_sage ships no prices.
// Payloads: e2e/mocks/value.ts.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { noRates, pricedCost, routeValue } from './mocks/value'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('market-04 CHECK-04: with rates the card shows dollars, source and date',
  async ({ page }) => {
    await routeValue(page, pricedCost)
    await page.goto('/#/')
    const card = page.getByRole('region', { name: 'Cost' })
    await expect(card.getByTestId('cost-source'))
      .toHaveText('Rates from config, as of 2026-11-01. Last 24 h.')
    await expect(card).toContainText('Total$54.00')
    await expect(card).toContainText('Compute$48.00')
    await expect(card).toContainText('Storage$6.00')
    await expect(card).toContainText('Idle capacity25.0%')
    const query = card.getByRole('table', { name: 'Top queries by cost' })
    const row = query.getByRole('row').filter({ hasText: '4242' })
    await expect(row).toContainText('75.0%')
    await expect(row).toContainText('$36.00')
    await expect(row.getByRole('link', { name: 'orders' }))
      .toHaveAttribute('href', '#/cases?database=orders')
  })

test('market-04 CHECK-04: without rates it says so and shows no figure', async ({ page }) => {
  await routeValue(page, noRates)
  await page.goto('/#/')
  const card = page.getByRole('region', { name: 'Cost' })
  await expect(card).toContainText('Rates not configured: set cost.rates')
  await expect(card).not.toContainText('$')
  await expect(card).not.toContainText('unknown')
  await expect(card.getByTestId('cost-source')).toHaveCount(0)
  await expect(card.getByRole('table')).toHaveCount(0)
})

test('a report with no cost section says why there is no figure', async ({ page }) => {
  await routeValue(page, { available: false, reason: 'no instance telemetry yet' })
  await page.goto('/#/')
  const card = page.getByRole('region', { name: 'Cost' })
  await expect(card).toContainText('Cost unavailable: no instance telemetry yet.')
  await expect(card).not.toContainText('$')
})
