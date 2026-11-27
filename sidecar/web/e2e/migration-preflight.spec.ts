// migration-preflight.spec.ts (dashboard-18): the DDL preflight's
// measured_at panel on a migration case (migration-01,
// reports/t1/migration-01.md:262 MANUAL "Web preflight measured_at panel").
// Payloads: e2e/mocks/lifecycle.ts.
// No concurrency tests: the panel is a read-only render of one case.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { routeCases, routeFleet } from './mocks/cases'
import { NOW, UNMEASURED_CHECKS, migrationCase, plusMinutes } from './mocks/lifecycle'

async function preflightOf(page: Page, preflight: Record<string, unknown>) {
  const cases = [migrationCase(preflight)]
  await routeCases(page, () => ({ cases, total: 1 }))
  await page.goto('/#/cases')
  const card = page.getByRole('article')
  await card.getByTestId('case-technical-details').locator('summary').click()
  const panel = card.locator('section').filter({ hasText: 'DDL preflight' })
  await expect(panel).toContainText('ACCESS EXCLUSIVE rewrite risk')
  return panel
}

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW)
  await mockAllAPIs(page)
  await routeFleet(page)
})

test('migration-01: the preflight says its live values were measured at detection',
  async ({ page }) => {
    const measured = plusMinutes(-5)
    const panel = await preflightOf(page, { measured_at: measured })
    const note = panel.getByTestId('preflight-measured-at')
    await expect(note).toHaveText('Measured at detection, 5 min ago')
    await expect(note).toHaveAttribute('title', measured)
    await expect(panel).toContainText('Lock: ACCESS EXCLUSIVE')
    await expect(panel).toContainText('Rewrite: full table rewrite')
    await expect(panel.getByTestId('preflight-not-measured')).toHaveCount(0)
  })

for (const [minutes, label] of [
  [0, 'just now'], [-1, '1 min ago'], [-60, '1 h ago'], [-2 * 24 * 60, '2 d ago'],
  // A measurement ahead of the browser clock (skew) is never a future age.
  [5, 'just now'],
] as const) {
  test(`measured ${minutes} min from now reads "${label}"`, async ({ page }) => {
    const panel = await preflightOf(page, { measured_at: plusMinutes(minutes) })
    await expect(panel.getByTestId('preflight-measured-at'))
      .toHaveText(`Measured at detection, ${label}`)
  })
}

test('a preflight with no measured_at says "not measured", not an epoch date',
  async ({ page }) => {
    const panel = await preflightOf(page, { checks: UNMEASURED_CHECKS })
    await expect(panel.getByTestId('preflight-not-measured'))
      .toHaveText('Live conditions were not measured at detection.')
    await expect(panel.getByTestId('preflight-measured-at')).toHaveCount(0)
    await expect(panel).not.toContainText(/1970|d ago/)
  })

test('an unparseable measured_at is treated as not measured', async ({ page }) => {
  const panel = await preflightOf(page, { measured_at: 'yesterday', checks: UNMEASURED_CHECKS })
  await expect(panel.getByTestId('preflight-measured-at')).toHaveCount(0)
  await expect(panel.getByTestId('preflight-not-measured')).toBeVisible()
})

test('a preflight measured by another source claims nothing about live values',
  async ({ page }) => {
    // A known live check without a time: neither panel line is drawn.
    const panel = await preflightOf(page, {})
    await expect(panel.getByTestId('preflight-measured-at')).toHaveCount(0)
    await expect(panel.getByTestId('preflight-not-measured')).toHaveCount(0)
  })
