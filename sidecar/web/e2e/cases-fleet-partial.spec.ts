// cases-fleet-partial.spec.ts (dashboard-13): a stopped fleet database
// gives a banner on Cases while the other databases' cases still show
// (cases-05 CHECK-12), and the Fleet page of a metadata fleet with one
// database stopped (fleet-06 CHECK-04), in the real bundle with real
// routing. Payloads: e2e/mocks/cases.ts. The live versions (a real stopped
// fleet member) are OWNER ACTIONs in dashboard-17.
// No concurrency tests: polling is covered by dashboard-16.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { partialCases, routeCases, routeFleet } from './mocks/cases'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('cases-05 CHECK-12: a stopped database gives a banner and the other cases show',
  async ({ page }) => {
    await routeFleet(page, () => false)
    await routeCases(page, () => partialCases(true))
    await page.goto('/#/cases')

    const banner = page.getByTestId('cases-unavailable')
    await expect(banner).toHaveCount(1)
    await expect(banner).toHaveText('Issues for database reporting could not be reached. ' +
      'Its issues are missing from this list; the other databases are shown.')
    await expect(page.getByRole('article')).toHaveCount(1)
    await expect(page.getByTestId('case-database')).toHaveText('DB: orders')
  })

test('an unknown error class still names the database in the banner', async ({ page }) => {
  await routeFleet(page, () => false)
  await routeCases(page, () => ({
    ...partialCases(true), unavailable: [{ database: 'reporting', error_class: 'novel' }],
  }))
  await page.goto('/#/cases')
  await expect(page.getByTestId('cases-unavailable'))
    .toContainText('Issues for database reporting could not be read.')
  await expect(page.getByTestId('case-database')).toHaveText('DB: orders')
})

test('a database coming back on the next poll removes the banner', async ({ page }) => {
  await page.clock.install()
  let reportingDown = true
  await routeFleet(page, () => !reportingDown)
  await routeCases(page, () => partialCases(reportingDown))
  await page.goto('/#/cases')
  await expect(page.getByTestId('cases-unavailable')).toContainText('reporting')
  await expect(page.getByRole('article')).toHaveCount(1)

  reportingDown = false
  // The Cases list polls every 30 s (CasesPage useAPI interval).
  await page.clock.fastForward(30_000)
  await expect(page.getByTestId('cases-unavailable')).toHaveCount(0)
  await expect(page.getByTestId('case-database')).toHaveText(['DB: orders', 'DB: reporting'])
})

test('fleet-06 CHECK-04: a metadata fleet with one database stopped', async ({ page }) => {
  await routeFleet(page, () => false)
  await page.goto('/#/manage-databases')
  await expect(page.getByTestId('databases-table')).toBeVisible()
  const rows = page.getByTestId('db-row')
  await expect(rows).toHaveCount(2)

  const reporting = rows.filter({ hasText: 'reporting.db' })
  await expect(reporting.getByTestId('runtime-connection')).toHaveText('disconnected')
  await expect(reporting.getByTestId('runtime-health')).toHaveText('0not connected')
  // Stopped is a runtime state: the registration stays enabled, not "disabled".
  await expect(reporting.getByTestId('db-registration')).toHaveText('')

  const orders = rows.filter({ hasText: 'orders.db' })
  await expect(orders.getByTestId('runtime-connection')).toHaveText('connected')
  await expect(orders.getByTestId('runtime-findings')).toContainText('2 open')
})
