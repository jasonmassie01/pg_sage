// why-not.spec.ts (ux-08b): the Why not panel opened from a case, a
// database row on Fleet and an empty Inbox, in the real bundle with real
// routing and clicks. The chain copies TestWhyNotApprovalRequiredLinksTheCard
// (internal/api/whynot_db_test.go) plus a privilege failure with its GRANT.
// No concurrency tests: each test opens one panel.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { findingCase, routeCases, routeFleet } from './mocks/cases'
import { routeInbox } from './mocks/inbox'

const NAMES = ['observed', 'finding', 'candidate', 'policy', 'window', 'budget', 'privilege',
  'trust', 'verification_wait', 'emergency_stop']
const GRANT = 'GRANT pg_read_all_stats TO sage;'

function chain(database: string) {
  const links = NAMES.map(name => ({ name, status: 'pass', sentence: `Checked ${name}.`,
    first_fail: false } as Record<string, unknown>))
  links[3] = { name: 'policy', status: 'fail', first_fail: true, reason: 'approval_required',
    sentence: 'Asks first: approval required.',
    fix: { kind: 'ui_link', value: `#/cases?finding=41&database=${database}` } }
  links[6] = { name: 'privilege', status: 'fail', first_fail: false,
    sentence: 'The sage role cannot read other roles\u2019 query text.',
    fix: { kind: 'grant_sql', value: GRANT, note: 'Run it as a superuser.' } }
  links[7] = { name: 'trust', status: 'unknown', first_fail: false,
    sentence: 'The gate stopped at another link.' }
  return { database, links, first_fail: 'policy', failing: ['policy', 'privilege'] }
}

// routeWhyNot answers GET /api/v1/whynot for the database asked about
// and records each request's query string.
async function routeWhyNot(page: Page) {
  const asked: string[] = []
  await page.route(url => url.pathname === '/api/v1/whynot', route => {
    const url = new URL(route.request().url())
    asked.push(url.search)
    return route.fulfill({ json: chain(url.searchParams.get('database') || '') })
  })
  return asked
}

async function expectChain(page: Page) {
  const panel = page.getByTestId('why-not-panel')
  await expect(panel.getByTestId('why-not-link')).toHaveCount(10)
  await expect(panel.getByTestId('why-not-summary'))
    .toHaveText('Held back at Policy check: Asks first: approval required.')
  const first = panel.locator('[data-testid="why-not-link"][data-first-fail="true"]')
  await expect(first).toHaveCount(1)
  await expect(first).toHaveAttribute('data-link', 'policy')
  await expect(panel.locator('[data-link="trust"]').getByTestId('why-not-status'))
    .toHaveText('Unknown')
  await expect(panel.locator('[data-link="privilege"]')).toContainText(GRANT)
  return panel
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('ux-08b: the panel opens from a case for its finding', async ({ page }) => {
  await routeFleet(page)
  const asked = await routeWhyNot(page)
  await routeCases(page, () => ({ cases: [findingCase('41', 'orders')], total: 1 }))
  await page.goto('/#/cases')
  const card = page.getByRole('article')
  await expect(card).toHaveCount(1)
  await card.getByText('Technical evidence and proposed changes').click()
  await card.getByRole('button', { name: 'Why not?' }).click()
  const panel = await expectChain(page)
  await expect(panel.locator('[data-link="policy"]').getByRole('link', { name: /open/i }))
    .toHaveAttribute('href', '#/cases?finding=41&database=orders')
  expect(asked).toContain('?database=orders&finding_id=41')
})

test('ux-08b: the panel opens from a database row on Fleet', async ({ page }) => {
  await routeFleet(page)
  const asked = await routeWhyNot(page)
  await page.goto('/#/manage-databases')
  const rows = page.getByTestId('db-row')
  await expect(rows).toHaveCount(2)
  await rows.nth(1).getByRole('button', { name: 'Why not?' }).click()
  await expectChain(page)
  expect(asked).toEqual(['?database=reporting'])
  await rows.nth(1).getByRole('button', { name: 'Why not?' }).click()
  await expect(page.getByTestId('why-not-panel')).toHaveCount(0)
})

test('ux-08b: an empty Inbox asks why nothing was proposed', async ({ page }) => {
  await routeInbox(page, { items: [], rows: [] })
  const asked = await routeWhyNot(page)
  await page.goto('/#/')
  await expect(page.getByTestId('inbox-goal')).toHaveText('Nothing needs you.')
  await page.getByRole('button', { name: 'Nothing proposed? See why' }).click()
  await expectChain(page)
  expect(asked).toEqual(['?database=primary'])
})

test('ux-08b: a failed read shows the error and no chain', async ({ page }) => {
  await routeFleet(page)
  await page.route(url => url.pathname === '/api/v1/whynot', route =>
    route.fulfill({ status: 500, json: { error: 'internal error' } }))
  await page.goto('/#/manage-databases')
  await page.getByTestId('db-row').nth(0).getByRole('button', { name: 'Why not?' }).click()
  const panel = page.getByTestId('why-not-panel')
  await expect(panel.getByRole('alert')).toContainText('internal error')
  await expect(panel.getByTestId('why-not-link')).toHaveCount(0)
})
