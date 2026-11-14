// slo-badge.spec.ts (dashboard-15): the SLOs page badges a Prometheus SLO
// whose window coverage is unverified (playbooks-07 CHECK-09) and leaves a
// push SLO unbadged. Payloads: e2e/mocks/trust.ts.
// No concurrency tests: each test drives one page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { routeSLOs, slo } from './mocks/trust'

function sloRow(page: Page, name: string) {
  return page.locator('tr').filter({ has: page.getByText(name, { exact: true }) })
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('playbooks-07 CHECK-09: a Prometheus SLO without coverage_query is badged',
  async ({ page }) => {
    await routeSLOs(page, [slo('search', 'prometheus', true), slo('checkout', 'push', false)])
    await page.goto('/#/slos')
    const badge = sloRow(page, 'search').getByTestId('slo-coverage-unverified')
    await expect(badge).toHaveText('coverage unverified')
    await expect(badge).toHaveAttribute('title',
      'No coverage_query: Prometheus windows are taken as fully covered and fresh')
    await expect(sloRow(page, 'search').getByTestId('slo-state')).toBeVisible()
    await expect(sloRow(page, 'checkout').getByTestId('slo-coverage-unverified'))
      .toHaveCount(0)
    await expect(page.getByTestId('slo-coverage-unverified')).toHaveCount(1)
  })

test('no SLOs shows the empty state and no badge', async ({ page }) => {
  await routeSLOs(page, [])
  await page.goto('/#/slos')
  await expect(page.getByTestId('slo-empty')).toContainText('No SLOs evaluated yet')
  await expect(page.getByTestId('slo-coverage-unverified')).toHaveCount(0)
})

test('a failed SLO read shows the error, not the empty state', async ({ page }) => {
  await page.route(url => url.pathname === '/api/v1/sre/slos', route =>
    route.fulfill({ status: 503, json: { error: 'slo evaluator unavailable' } }))
  await page.goto('/#/slos')
  await expect(page.getByRole('alert').filter({ hasText: 'SLOs unavailable' }))
    .toContainText('SLOs unavailable: slo evaluator unavailable (503)')
  await expect(page.getByTestId('slo-empty')).toHaveCount(0)
})
