// policy-sets.spec.ts (fleetpol-01c): Settings > Policy sets lists the
// sets and the exceptions with their diff, proposes a set change through
// the existing propose-then-ratify routes, and the database page says
// which set governs the database. Payloads: e2e/mocks/policy.ts, copied
// from internal/api/testdata/policy_sets_golden.json.
// No concurrency tests: one person drives one page; the store's concurrent
// writes are policy.TestPolicySetDB_*.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs, mockDatabases } from './fixtures'
import { policyException, policyState, routePolicy, type PolicyState } from './mocks/policy'

test.use({ timezoneId: 'UTC' })

async function openPolicySets(page: Page) {
  await page.goto('/#/settings')
  await page.getByTestId('settings-mode-toggle').click()
  await page.getByTestId('settings-tab-policy-sets').click()
}

function setCard(page: Page, name: string) {
  return page.locator(`[data-testid="policy-set"][data-set="${name}"]`)
}

async function withState(page: Page, over: Partial<PolicyState> = {}) {
  const state = policyState(over)
  await routePolicy(page, state)
  return state
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.removeItem('pg_sage_settings_mode'))
})

test('lists two sets and renders the exception diff', async ({ page }) => {
  await withState(page)
  await openPolicySets(page)
  await expect(page.getByTestId('policy-set')).toHaveCount(2)
  const prod = setCard(page, 'prod')
  await expect(prod.getByTestId('policy-set-version')).toHaveText(/Version 2/)
  await expect(prod.getByTestId('policy-set-selectors')).toContainText('tier=prod')
  await expect(prod.getByTestId('policy-set-members'))
    .toContainText('2 databases (1 with an exception)')
  await expect(prod.getByTestId('policy-set-requirements')).toContainText('index: advisory')
  const row = page.getByTestId('policy-exception')
  await expect(row).toHaveCount(1)
  await expect(row).toContainText('billing')
  await expect(row).toContainText('alice@example.com')
  await expect(row.getByTestId('policy-exception-diff')).toContainText('120000')
  await prod.getByTestId('policy-set-yaml-open').click()
  await expect(prod.getByTestId('policy-set-yaml')).toContainText('tier: prod')
})

test('a 400 on propose shows the server message', async ({ page }) => {
  const state = await withState(page, { proposeStatus: 400,
    proposeError: 'policy document: unknown field "lock_ceiling"' })
  await openPolicySets(page)
  const prod = setCard(page, 'prod')
  await prod.getByTestId('policy-set-propose').click()
  await prod.getByTestId('policy-propose-submit').click()
  await expect(prod.getByRole('alert')).toContainText('unknown field "lock_ceiling"')
  expect(state.sent).toHaveLength(1)
  expect(state.sent[0].body).toMatchObject({ set_name: 'prod', expected_version: 2 })
})

test('no sets explains the default set', async ({ page }) => {
  await withState(page, { sets: [], exceptions: [] })
  await openPolicySets(page)
  await expect(page.getByTestId('policy-sets-empty')).toContainText('default')
  await expect(page.getByTestId('policy-exceptions-empty')).toBeVisible()
})

test('an API 500 shows the error through QueryState', async ({ page }) => {
  await withState(page, { setsStatus: 500 })
  await openPolicySets(page)
  await expect(page.getByTestId('policy-sets').getByRole('alert'))
    .toContainText('read policy sets: connection refused')
  await expect(page.getByTestId('policy-set')).toHaveCount(0)
})

test('an exception expiring today is marked, one without expiry is flagged', async ({ page }) => {
  await withState(page, { exceptions: [
    policyException({ expires_at: '2026-11-17T23:59:00Z' }),
    policyException({ database_id: 4, expires_at: undefined }),
  ] })
  // The clock starts at noon UTC on 2026-11-17 and keeps running.
  await page.clock.install({ time: new Date('2026-11-17T12:00:00Z') })
  await openPolicySets(page)
  const rows = page.getByTestId('policy-exception')
  await expect(rows.nth(0).getByTestId('policy-exception-expires-today')).toBeVisible()
  await expect(rows.nth(1).getByTestId('policy-exception-no-expiry')).toBeVisible()
  await expect(rows.nth(1)).toContainText('orders')
  await expect(page.getByTestId('policy-exceptions-unbounded')).toContainText('1')
})

test('after ratify the version number changes and the diff disappears', async ({ page }) => {
  const state = await withState(page)
  await openPolicySets(page)
  const prod = setCard(page, 'prod')
  await prod.getByTestId('policy-set-propose').click()
  await prod.getByTestId('policy-propose-submit').click()
  await expect(prod.getByTestId('policy-propose-filed')).toContainText('#77')
  await prod.getByTestId('policy-propose-ratify').click()
  await expect(prod.getByTestId('policy-set-version')).toHaveText(/Version 3/)
  await expect(page.getByTestId('policy-exception-diff')).toHaveCount(0)
  await expect(page.getByTestId('policy-exception-same')).toBeVisible()
  expect(state.sent.map(s => s.path)).toEqual(
    ['/api/v1/policy/proposals', '/api/v1/policy/proposals/77/ratify'])
})

test('owner names render as text, never HTML', async ({ page }) => {
  const evil = '<img src=x onerror="window.__pwned=1">'
  await withState(page, { exceptions: [policyException({ set_by: evil })] })
  await openPolicySets(page)
  await expect(page.getByTestId('policy-exception')).toContainText(evil)
  await expect(page.getByTestId('policy-exceptions').locator('img')).toHaveCount(0)
  expect(await page.evaluate(() => (window as { __pwned?: number }).__pwned)).toBeUndefined()
})

test('the database page says which set governs it and why', async ({ page }) => {
  await withState(page)
  const databases = mockDatabases.databases.map((d, i) => ({ ...d, id: i === 0 ? 9 : 4 }))
  await page.route('**/api/v1/databases', route =>
    route.fulfill({ json: { ...mockDatabases, databases } }))
  await page.goto('/#/database')
  await page.getByRole('combobox', { name: 'Database scope' }).selectOption('primary')
  await expect(page.getByTestId('governed-by')).toHaveText(
    'Governed by: prod (exception: lock_duration_ceiling_ms changed by alice@example.com, '
    + 'expires 2026-12-01)')
})
