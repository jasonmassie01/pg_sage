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
  await expect(badge)
    .toHaveText(/^Fleet stopped by operator@test\.com at \d{4}-\d\d-\d\d \d\d:\d\d \S+$/)
  await expect(badge.locator('time')).toHaveAttribute('datetime', STOPPED_AT)
})

// ux-05d (win 15): the time names its zone, and the header zone toggle
// rewrites it in UTC and remembers the choice across a reload.
test('the stop time names its zone and follows the header UTC toggle', async ({ page }) => {
  await routeStop(page, () => ({ fleetStopped: true, stoppedBy: 'operator@test.com' }))
  await page.goto('/#/')
  const badge = page.getByTestId('emergency-stop-badge')
  await expect(badge).toContainText('Fleet stopped by operator@test.com at')
  await page.getByRole('combobox', { name: 'Time zone' }).selectOption('utc')
  await expect(badge).toHaveText('Fleet stopped by operator@test.com at 2026-11-13 07:30 UTC')
  await page.reload()
  await expect(page.getByTestId('emergency-stop-badge'))
    .toHaveText('Fleet stopped by operator@test.com at 2026-11-13 07:30 UTC')
  await expect(page.getByRole('combobox', { name: 'Time zone' })).toHaveValue('utc')
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
