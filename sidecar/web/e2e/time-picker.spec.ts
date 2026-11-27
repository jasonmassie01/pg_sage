// time-picker.spec.ts (dashboard-14): the header time picker shows only on
// the pages it drives, captioned with what it scopes (dashboard-07
// CHECK-10): none on #/slos, "Ledger window" on #/actions, and the chosen
// window reaches the ledger request.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('dashboard-07 CHECK-10: no time picker on #/slos', async ({ page }) => {
  await page.goto('/#/slos')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('SLOs')
  await expect(page.getByTestId('time-range-picker')).toHaveCount(0)
})

test('dashboard-07 CHECK-10: #/actions reads "Ledger window"', async ({ page }) => {
  await page.goto('/#/actions')
  // ux-11: Actions is Changes at #/changes; the old path redirects.
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Changes')
  await expect(page.getByTestId('time-range-scope')).toHaveText('Ledger window')
  await expect(page.getByLabel('Time range')).toHaveValue('24h')
})

test('moving from #/actions to #/slos removes the picker, and back restores it',
  async ({ page }) => {
    await page.goto('/#/actions')
    await expect(page.getByTestId('time-range-scope')).toHaveText('Ledger window')
    // ux-11: SLOs is in the collapsible Evidence group.
    await page.getByTestId('sidebar-desktop').getByRole('button', { name: 'Evidence' }).click()
    await page.getByTestId('nav-slos').first().click()
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('SLOs')
    await expect(page.getByTestId('time-range-picker')).toHaveCount(0)
    await page.getByTestId('nav-actions').first().click()
    await expect(page.getByTestId('time-range-scope')).toHaveText('Ledger window')
  })

// ux-11: the health chart moved from Snapshot & metrics to the Database page.
test('the Database page names its health chart window', async ({ page }) => {
  await page.goto('/#/database?database=primary')
  await expect(page.getByTestId('time-range-scope')).toHaveText('Health chart window')
})

test('choosing 7 days asks the ledger for exactly a 7-day window', async ({ page }) => {
  const asked: URL[] = []
  await page.route(url => url.pathname === '/api/v1/actions', route => {
    asked.push(new URL(route.request().url()))
    return route.fulfill({ json: { actions: [], total: 0 } })
  })
  await page.goto('/#/actions')
  await expect.poll(() => asked.length).toBeGreaterThan(0)
  await page.getByLabel('Time range').selectOption('7d')
  await expect.poll(() => asked.filter(u => {
    const from = Date.parse(u.searchParams.get('from') || '')
    const to = Date.parse(u.searchParams.get('to') || '')
    return to - from === 7 * 24 * 60 * 60 * 1000
  }).length).toBeGreaterThan(0)
  const first = asked[0]
  const span = Date.parse(first.searchParams.get('to') || '')
    - Date.parse(first.searchParams.get('from') || '')
  expect(span, 'the first request uses the 24h default').toBe(24 * 60 * 60 * 1000)
})

test('an unknown stored range reads as the 24h default', async ({ page }) => {
  await page.addInitScript(() => {
    try {
      localStorage.setItem('pg_sage_range', '90d')
    } catch {
      // storage blocked: the default applies anyway
    }
  })
  await page.goto('/#/actions')
  await expect(page.getByLabel('Time range')).toHaveValue('24h')
})
