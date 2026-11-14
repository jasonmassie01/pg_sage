// first-look.spec.ts (dashboard-16): the First look panel on the Value page
// (firstlook-01 CHECK-03: the GRANT that fixes a degraded check in a <pre>;
// rca-09 CHECK-30: the log_rca_unavailable item with its steps) and the
// provider readiness matrix (tuning-04 CHECK-07: the hint_plan limitation
// line). Payloads: e2e/mocks/firstlook.ts.
// No concurrency tests: both panels are read-only renders of one response.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  HINT_LIMITATION, LOG_RCA_DETAIL, LOG_RCA_STEPS, OK_CHECKS, SEQUENCE_GRANT, logRCAItem,
  readinessRow, report, routeFirstLook, routeReadiness, sequenceRunwayDegraded,
} from './mocks/firstlook'

function firstLook(page: Page) {
  return page.getByRole('region', { name: 'First look' })
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('firstlook-01 CHECK-03: a degraded check shows its GRANT in a <pre>', async ({ page }) => {
  await routeFirstLook(page, () => ({ json: { databases: [{ database: 'app',
    report: report('app', { checks: [...OK_CHECKS, sequenceRunwayDegraded] }) }] } }))
  await page.goto('/#/')
  const panel = firstLook(page)
  await expect(panel).toContainText('app: 1,240 relations read in 300 ms, catalog only.')
  // An empty item list with a check that could not run is not "healthy".
  await expect(panel).toContainText('No problems found in the 3 checks that ran; 1 could not run.')
  await expect(panel).not.toContainText('No problems found in the catalog.')
  const strip = panel.getByRole('region', { name: 'Catalog checks' })
  await expect(strip).toContainText('4 checks, 1 could not run.')
  const row = strip.getByRole('listitem')
  await expect(row).toHaveCount(1)
  await expect(row).toContainText('could not run')
  await expect(row).toContainText('sequence_runway')
  await expect(row.locator('p')).toHaveText(
    '3 sequences are not readable by the pg_sage role, so their runway is unknown')
  const grant = row.locator('pre')
  await expect(grant).toHaveText(SEQUENCE_GRANT)
  expect(await grant.evaluate(el => el.tagName)).toBe('PRE')
})

test('rca-09 CHECK-30: a stderr-logging server shows log_rca_unavailable with the steps',
  async ({ page }) => {
    await routeFirstLook(page, () => ({ json: { databases: [{ database: 'app',
      report: report('app', { items: [logRCAItem] }) }] } }))
    await page.goto('/#/')
    const card = firstLook(page).getByRole('article')
      .filter({ hasText: "Log-based RCA cannot read this server's log" })
    await expect(card).toHaveCount(1)
    await expect(card.getByRole('heading', { level: 3 }))
      .toHaveText("Log-based RCA cannot read this server's log")
    await expect(card).toContainText('info')
    await expect(card).toContainText(LOG_RCA_DETAIL)
    await expect(card).toContainText(LOG_RCA_STEPS.join(' '))
    // Steps are for the operator: this item suggests no SQL for pg_sage to run.
    await expect(card.locator('pre')).toHaveCount(0)
    await card.getByText('Evidence').click()
    await expect(card).toContainText(
      'log_destination, logging_collector, lc_messages — log_rca status missing')
  })

test('no first-look items with every check run shows the empty state', async ({ page }) => {
  await routeFirstLook(page, () => ({ json: { databases: [{ database: 'app',
    report: report('app') }] } }))
  await page.goto('/#/')
  const panel = firstLook(page)
  await expect(panel).toContainText('No problems found in the catalog.')
  await expect(panel.getByRole('article')).toHaveCount(0)
  await expect(panel.getByRole('region', { name: 'Catalog checks' })).toHaveCount(0)
})

test('no databases renders no panel; a failed read and an unreachable database say so',
  async ({ page }) => {
    let answer: { status?: number, json: unknown } = { json: { databases: [] } }
    await routeFirstLook(page, () => answer)
    await page.goto('/#/')
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Value')
    await expect(page.getByRole('heading', { name: 'First look' })).toHaveCount(0)

    answer = { status: 503, json: { error: 'first look store unavailable' } }
    await page.reload()
    await expect(firstLook(page)).toContainText('first look store unavailable (503)')

    answer = { json: { databases: [{ database: 'orders', report: null, error: 'unreachable' }] } }
    await page.reload()
    await expect(firstLook(page)).toContainText(
      'pg_sage cannot reach the database orders; check the connection URL and network.')
  })

test('tuning-04 CHECK-07: provider readiness shows the hint_plan limitation line',
  async ({ page }) => {
    await routeReadiness(page, [readinessRow('primary', false), readinessRow('replica', true)])
    await page.goto('/#/advanced')
    await page.getByTestId('overview-tab-provider-readiness').click()
    const matrix = page.getByTestId('provider-readiness-matrix')
    const blocked = matrix.getByRole('row').filter({ hasText: 'primary' })
    await expect(blocked.getByRole('cell').nth(5)).toHaveText('available')
    await expect(blocked.getByRole('cell').last()).toHaveText(HINT_LIMITATION)
    // Hints that reach sessions carry no limitation: the column says none.
    const ready = matrix.getByRole('row').filter({ hasText: 'replica' })
    await expect(ready.getByRole('cell').last()).toHaveText('none')
    await expect(matrix).not.toContainText(/replica.*do not reach/)
  })
