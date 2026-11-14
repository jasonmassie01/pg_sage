// audit-card.spec.ts (dashboard-15): the Settings audit card shows a chain
// that fails verification as a red row naming row_edited (audit-12 CHECK-29,
// the card half; the live tamper and pg_sage_audit_chain_ok are OWNER ACTION
// in dashboard-17), and follows the chain from ok to broken to ok.
// Payloads: e2e/mocks/settings.ts.
// No concurrency tests: each test drives one page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { VERIFIED_AT, routeAudit } from './mocks/settings'

const CHAIN = 'audit-chain-orders-action_log'

// cssColor resolves a theme variable to the rgb() string getComputedStyle reports.
function cssColor(page: Page, variable: string) {
  return page.evaluate(v => {
    const probe = document.createElement('span')
    probe.style.color = `var(${v})`
    document.body.appendChild(probe)
    const color = getComputedStyle(probe).color
    probe.remove()
    return color
  }, variable)
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.removeItem('pg_sage_settings_mode'))
})

test('audit-12 CHECK-29: an edited row shows the chain red with row_edited',
  async ({ page }) => {
    await routeAudit(page, () => false)
    await page.goto('/#/settings')
    const row = page.getByTestId('audit-card').getByTestId(CHAIN)
    await expect(row).toBeVisible()
    const red = await cssColor(page, '--red')
    expect(red).not.toBe(await cssColor(page, '--green'))
    // data-ok="false" is the card's side of pg_sage_audit_chain_ok = 0.
    await expect(row).toHaveAttribute('data-ok', 'false')
    const state = row.getByTestId('audit-chain-state')
    await expect(state).toHaveText('fails verification')
    await expect(state).toHaveCSS('color', red)
    await expect(row.getByText('row_edited ×1')).toHaveCSS('color', red)
    await expect(row).toContainText(`full · head 9 · last verified ${VERIFIED_AT}`)
  })

test('the chain goes from verified to broken to verified on the next polls',
  async ({ page }) => {
    let ok = true
    await page.clock.install()
    await routeAudit(page, () => ok)
    await page.goto('/#/settings')
    const row = page.getByTestId(CHAIN)
    await expect(row).toHaveAttribute('data-ok', 'true')
    await expect(row.getByTestId('audit-chain-state')).toHaveText('verified')
    await expect(row).not.toContainText('row_edited')

    ok = false
    // The audit card polls every 60 s (useAPI interval).
    await page.clock.fastForward(60_000)
    await expect(row).toHaveAttribute('data-ok', 'false')
    await expect(row).toContainText('row_edited ×1')

    ok = true
    await page.clock.fastForward(60_000)
    await expect(row).toHaveAttribute('data-ok', 'true')
    await expect(row.getByTestId('audit-chain-state')).toHaveText('verified')
    await expect(row).not.toContainText('row_edited')
  })

test('no chain verified yet says when verification runs', async ({ page }) => {
  await page.route(url => url.pathname === '/api/v1/audit/status', route =>
    route.fulfill({ json: { ok: true, verify_interval_hours: 24, chains: [],
      siem: { enabled: false }, evidence: { signed: false } } }))
  await page.goto('/#/settings')
  await expect(page.getByTestId('audit-not-verified'))
    .toHaveText('No chain verified since this sidecar started. Scheduled verification '
      + 'runs every 24 h, on the fleet leader only.')
  await expect(page.locator(`[data-testid="${CHAIN}"]`)).toHaveCount(0)
})

test('a failed status read shows the error, not a verified card', async ({ page }) => {
  await page.route(url => url.pathname === '/api/v1/audit/status', route =>
    route.fulfill({ status: 503, json: { error: 'audit status unavailable' } }))
  await page.goto('/#/settings')
  await expect(page.getByRole('alert').filter({ hasText: 'Audit trail:' }))
    .toContainText('Audit trail: audit status unavailable (503)')
  await expect(page.getByTestId('audit-card')).toHaveCount(0)
})
