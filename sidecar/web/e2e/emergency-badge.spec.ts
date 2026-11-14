// emergency-badge.spec.ts (dashboard-14): the header emergency stop badge
// names a fleet stop and who pressed it (fleet-09 CHECK-04), and appears and
// disappears with emergency_stopped on the next fleet poll.
// Payloads: e2e/mocks/autonomy.ts.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { STOPPED_AT, routeStop, type StopState } from './mocks/autonomy'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('fleet-09 CHECK-04: the fleet stop badge names who pressed it', async ({ page }) => {
  await routeStop(page, () => ({ fleetStopped: true, stoppedBy: 'operator@test.com' }))
  await page.goto('/#/')
  const badge = page.getByTestId('emergency-stop-badge')
  const at = await page.evaluate(iso => new Date(iso).toLocaleString(), STOPPED_AT)
  await expect(badge).toHaveText(`Fleet stopped by operator@test.com at ${at}`)
  await expect(badge.locator('time')).toHaveAttribute('datetime', STOPPED_AT)
})

test('a fleet stop with no recorded presser reads "unknown"', async ({ page }) => {
  await routeStop(page, () => ({ fleetStopped: true, stoppedBy: '' }))
  await page.goto('/#/')
  await expect(page.getByTestId('emergency-stop-badge'))
    .toContainText('Fleet stopped by unknown at')
})

test('a one-database stop names the database, not the fleet', async ({ page }) => {
  await routeStop(page, () => ({ fleetStopped: false, databaseStoppedBy: 'op@test.com' }))
  await page.goto('/#/')
  const badge = page.getByTestId('emergency-stop-badge')
  await expect(badge).toContainText('orders STOPPED by op@test.com at')
  await expect(badge).not.toContainText('Fleet stopped')
})

test('the badge appears and disappears with emergency_stopped', async ({ page }) => {
  await page.clock.install()
  let state: StopState = { fleetStopped: false }
  await routeStop(page, () => state)
  await page.goto('/#/')
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
  await expect(page.getByTestId('emergency-stop-badge')).toHaveCount(0)

  state = { fleetStopped: true, stoppedBy: 'operator@test.com' }
  // The fleet inventory polls every 30 s (App useAPI interval).
  await page.clock.fastForward(30_000)
  await expect(page.getByTestId('emergency-stop-badge'))
    .toContainText('Fleet stopped by operator@test.com')

  state = { fleetStopped: false }
  await page.clock.fastForward(30_000)
  await expect(page.getByTestId('emergency-stop-badge')).toHaveCount(0)
})
