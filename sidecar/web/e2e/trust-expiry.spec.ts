// trust-expiry.spec.ts (dashboard-15): the Trust page says until when an
// earned L3 holds (earned-06 CHECK-20) and shows a self-initiated L3 capped
// by a PostgreSQL major upgrade, with Re-approve (earned-07 CHECK-23, the
// browser half; the pg_upgrade half is OWNER ACTION in dashboard-17).
// Payloads: e2e/mocks/trust.ts.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  BENCH_EXPIRES, MAJOR_CAP_TEXT, TRUST_MEANING, incidentRow, routeTrust, selfRow, trustBody,
} from './mocks/trust'

const FREEZE = 'trust-row-orders-wraparound_runway-freeze'
const VACUUM = 'trust-row-orders-hygiene-vacuum'
const HOLDS = `L3 holds until ${BENCH_EXPIRES.slice(0, 10)} unless a new bench report arrives`

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('earned-06 CHECK-20: an L3 whose report is 24 days old says until when it holds',
  async ({ page }) => {
    await routeTrust(page, () => ({ json: trustBody([incidentRow(24)]) }))
    await page.goto('/#/trust')
    const row = page.getByTestId(FREEZE)
    await expect(row.getByTestId('trust-holds-until')).toHaveText(HOLDS)
    await expect(row.getByTestId('trust-level')).toHaveText('L3')
    await expect(row.getByTestId('trust-effective')).toHaveText('L3')
    await expect(row.getByTestId('trust-bench')).toHaveText('bench: ci (signed)')
  })

test('the date comes from the server: a 23-day-old report names the same day',
  async ({ page }) => {
    // Service.holding sends holds_until for every held L2+ incident level,
    // whatever the report's age; only the evidence_expiring notification
    // waits for the last 7 days (ExpiryWarningLead), and the page shows
    // the date so a lapse is never a surprise.
    await routeTrust(page, () => ({ json: trustBody([incidentRow(23)]) }))
    await page.goto('/#/trust')
    await expect(page.getByTestId(FREEZE).getByTestId('trust-holds-until')).toHaveText(HOLDS)
  })

test('at 30 days the report has aged out: no holds-until line, now L2', async ({ page }) => {
  await routeTrust(page, () => ({ json: trustBody([incidentRow(30)]) }))
  await page.goto('/#/trust')
  const row = page.getByTestId(FREEZE)
  await expect(row.getByTestId('trust-effective')).toHaveText('L2')
  await expect(row.getByTestId('trust-level')).toHaveText('L3')
  await expect(row.getByTestId('trust-holds-until')).toHaveCount(0)
})

test('earned-07 CHECK-23: a major-version cap shows the reason and Re-approve lifts it',
  async ({ page }) => {
    let capped = true
    const revalidations = await routeTrust(page,
      () => ({ json: trustBody([selfRow(capped)]) }), () => { capped = false })
    await page.goto('/#/trust')
    const row = page.getByTestId(VACUUM)
    await expect(row.getByTestId('trust-major-cap')).toContainText(MAJOR_CAP_TEXT)
    await expect(row.getByTestId('trust-effective')).toHaveText('L2')
    await expect(row.getByTestId('trust-cap')).toHaveText('L3')

    await row.getByRole('button', { name: 'Re-approve' }).click()
    await expect(row.getByTestId('trust-major-cap')).toHaveCount(0)
    await expect(row.getByTestId('trust-effective')).toHaveText('L3')
    expect(revalidations).toEqual([{
      url: expect.stringContaining('/api/v1/sre/autonomy/revalidate?database=orders'),
      body: { family: 'hygiene', class: 'vacuum', note: 're-approved on the Trust page' },
    }])
  })

test('an operator sees the cap but is not offered Re-approve', async ({ page }) => {
  await page.route('**/api/v1/auth/me', route =>
    route.fulfill({ json: { user: 'op', role: 'operator', email: 'op@test.com' } }))
  await routeTrust(page, () => ({ json: trustBody([selfRow(true)]) }))
  await page.goto('/#/trust')
  const row = page.getByTestId(VACUUM)
  await expect(row.getByTestId('trust-major-cap')).toContainText('PostgreSQL 16→17')
  await expect(row.getByRole('button', { name: 'Re-approve' })).toHaveCount(0)
})

test('no earned pairs shows the empty ledger, not a table', async ({ page }) => {
  await routeTrust(page, () => ({ json: trustBody([]) }))
  await page.goto('/#/trust')
  await expect(page.getByTestId('trust-page')).toContainText(TRUST_MEANING)
  await expect(page.getByTestId('trust-empty')).toContainText('No database has a trust ledger')
  await expect(page.getByTestId('trust-page').locator('table')).toHaveCount(0)
})

test('a 503 on /api/v1/trust shows the error with Retry, not an empty table',
  async ({ page }) => {
    let healthy = false
    await routeTrust(page, () => healthy
      ? { json: trustBody([incidentRow(24)]) }
      : { status: 503, json: { error: 'autonomy ledger unavailable',
        code: 'metadata_unavailable' } })
    await page.goto('/#/trust')
    const alert = page.getByRole('alert').filter({ hasText: 'autonomy ledger unavailable' })
    await expect(alert).toContainText('autonomy ledger unavailable (503)')
    await expect(page.getByTestId('trust-empty')).toHaveCount(0)
    await expect(page.getByTestId('trust-page')).toHaveCount(0)

    healthy = true
    await alert.getByRole('button', { name: /retry/i }).click()
    await expect(page.getByTestId(FREEZE).getByTestId('trust-holds-until')).toHaveText(HOLDS)
  })
