// slo-burn.spec.ts (dashboard-18): a burning proxy SLO on the SLOs page
// showing its duration, its state changes and its investigation
// (playbooks-09 CHECK-03, reports/t2/playbooks-09.md:98, UI half; the demo
// stack is dashboard-17 CHECK-18). Payloads: e2e/mocks/slo.ts.
// No concurrency tests: one page reads its own mocked responses.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  BURN_STARTED, NOW, SLO_NAME, proxySLO, routeSLOBurn, sloDetail, sloInvestigation,
  type Answer,
} from './mocks/slo'

const sloRow = (page: Page) =>
  page.locator('tr').filter({ has: page.getByText(SLO_NAME, { exact: true }) })

const list = (burning: boolean): Answer =>
  ({ json: { slos: [proxySLO(burning)], unavailable: [] } })

test.beforeEach(async ({ page }) => {
  await page.clock.install({ time: NOW })
  await mockAllAPIs(page)
})

test('playbooks-09 CHECK-03: a burning proxy SLO shows duration, history and investigation',
  async ({ page }) => {
    const reads = await routeSLOBurn(page, () => list(true), () => ({ json: sloDetail(true) }),
      () => ({ json: { items: [sloInvestigation] } }))
    await page.goto('/#/slos')
    const row = sloRow(page)
    await expect(row.getByTestId('slo-state')).toHaveText('Page')
    await expect(row.getByTestId('slo-state-age')).toHaveText('in page for 40m')
    await expect(row.getByTestId('slo-burn-start')).toHaveText('burning since 40m ago')
    await expect(row).toContainText('DB proxy · target 0.99 over 30d')
    await expect(row.getByTestId('slo-budget'))
      .toHaveText('partial window: 89% coveredspent so far: 12%')
    expect(reads).toEqual([])

    await row.getByTestId('slo-expand').click()
    const detail = page.getByTestId('slo-detail')
    await expect(detail.getByTestId('slo-transition')).toHaveText([
      'ticket → page · 40m ago', 'ok → ticket · 55m ago', 'first evaluation → ok · 3d ago'])
    await expect(detail.getByTestId('slo-recovery'))
      .toHaveText('Recovery: not recovered (still burning) · 12 slices evaluated')
    const panel = detail.getByTestId('investigation-panel')
    await expect(panel.getByTestId('investigation-state')).toHaveText('Running')
    await expect(panel).toContainText('slo_burn')
    // The detail asks for the recovery since the burn began; the lookup names
    // the SLO's own case on its database.
    const since = encodeURIComponent(BURN_STARTED)
    // The dev server's StrictMode may read twice; the set of reads is fixed.
    const asked = new Set(reads.map(u => new URL(u).pathname + new URL(u).search))
    expect([...asked].sort()).toEqual([
      `/api/v1/investigations?database=orders&case_id=slo%3A${SLO_NAME}&limit=1`,
      `/api/v1/sre/slos/${SLO_NAME}?database=orders&since=${since}`,
    ])
  })

test('the SLO goes from burning to recovered on the next poll and its badge changes',
  async ({ page }) => {
    let burning = true
    await routeSLOBurn(page, () => list(burning), () => ({ json: sloDetail(false) }),
      () => ({ json: { items: [] } }))
    await page.goto('/#/slos')
    const row = sloRow(page)
    await expect(row.getByTestId('slo-state')).toHaveText('Page')
    await expect(row.getByTestId('slo-burn-start')).toBeVisible()

    burning = false
    await page.clock.fastForward(30_000)
    await expect(row.getByTestId('slo-state')).toHaveText('OK')
    await expect(row.getByTestId('slo-burn-start')).toHaveCount(0)
    await expect(row.getByTestId('slo-state-age')).toHaveText('in ok for 1m')
    await expect(row.getByTestId('slo-budget'))
      .toHaveText('partial window: 89% coveredspent so far: 13%')
  })

test('a page-level burn with no investigation says why none opened', async ({ page }) => {
  await routeSLOBurn(page, () => list(true), () => ({ json: sloDetail(true) }),
    () => ({ json: { items: [] } }))
  await page.goto('/#/slos')
  await sloRow(page).getByTestId('slo-expand').click()
  await expect(page.getByTestId('slo-no-investigation')).toHaveText(
    'No investigation opened (sre.slo.open_investigations or automatic_start off)')
})

test('an unreadable investigator is "unknown", never "none opened"', async ({ page }) => {
  await routeSLOBurn(page, () => list(true), () => ({ json: sloDetail(true) }),
    () => ({ json: { items: [], unavailable: ['orders'] } }))
  await page.goto('/#/slos')
  await sloRow(page).getByTestId('slo-expand').click()
  await expect(page.getByTestId('slo-investigation-unavailable')).toHaveText(
    'Investigations of orders unavailable: whether one is open is unknown.')
  await expect(page.getByTestId('slo-no-investigation')).toHaveCount(0)
})

test('a failed detail read names the error and still looks up the investigation',
  async ({ page }) => {
    await routeSLOBurn(page, () => list(true),
      () => ({ status: 503, json: { error: 'slo history unavailable' } }),
      () => ({ json: { items: [sloInvestigation] } }))
    await page.goto('/#/slos')
    await sloRow(page).getByTestId('slo-expand').click()
    const detail = page.getByTestId('slo-detail')
    await expect(detail).toContainText('SLO detail unavailable: slo history unavailable (503)')
    await expect(detail.getByTestId('slo-transition')).toHaveCount(0)
    await expect(detail.getByTestId('investigation-panel')).toBeVisible()
  })

test('an SLO without a burn shows no burn line and no recovery', async ({ page }) => {
  await routeSLOBurn(page, () => list(false), () => ({ json: sloDetail(false) }),
    () => ({ json: { items: [] } }))
  await page.goto('/#/slos')
  const row = sloRow(page)
  await expect(row.getByTestId('slo-burn-start')).toHaveCount(0)
  await row.getByTestId('slo-expand').click()
  await expect(page.getByTestId('slo-transition')).toHaveCount(3)
  await expect(page.getByTestId('slo-recovery')).toHaveCount(0)
  await expect(page.getByTestId('slo-no-investigation')).toHaveText(
    'No investigation opened: only a page-level burn opens one.')
})
