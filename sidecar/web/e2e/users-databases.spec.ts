// users-databases.spec.ts (dashboard-16): apiauth-08 CHECK-05, the Users
// page Databases panel against a fleet sidecar. A user with grants is an
// operator only where granted and a viewer elsewhere; with none they keep
// their role everywhere; admins have no panel. Payloads: e2e/mocks/users.ts.
// No concurrency tests: one admin edits one user's grants in one tab.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { type Grant, type GrantCall, routeUsers } from './mocks/users'

const SCOPED = 'Limited to the databases below; a viewer on every other database.'
const NOT_ENFORCED =
  'Grants are saved but not enforced: set api.database_grants: true in fleet mode.'

function userRowFor(page: Page, email: string) {
  return page.getByTestId('users-table').getByRole('row').filter({ hasText: email })
}

async function openPanel(page: Page, email: string) {
  await page.goto('/#/users')
  await expect(page.getByTestId('users-table')).toContainText(email)
  await userRowFor(page, email).getByTestId('user-grants-button').click()
  const panel = page.getByTestId('user-grants-panel')
  await expect(panel.getByTestId('user-grants-notice')).toBeVisible()
  return panel
}

async function grantRows(panel: ReturnType<Page['getByTestId']>) {
  const rows = panel.getByTestId('user-grant-row')
  const out: string[] = []
  for (let i = 0; i < await rows.count(); i++) {
    const row = rows.nth(i)
    out.push(`${(await row.locator('span').first().innerText()).trim()}=` +
      `${await row.getByTestId('user-grant-role').inputValue()}`)
  }
  return out
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('apiauth-08 CHECK-05: the Databases panel lists, adds, changes and removes grants',
  async ({ page }) => {
    const grants = new Map<number, Grant[]>([[2, [{ database: 'primary', role: 'operator' }]]])
    const calls: GrantCall[] = []
    await routeUsers(page, grants, { calls })
    const panel = await openPanel(page, 'ops@test.com')
    await expect(panel.getByTestId('user-grants-notice')).toHaveText(SCOPED)
    await expect.poll(() => grantRows(panel)).toEqual(['primary=operator'])
    await expect(userRowFor(page, 'ops@test.com').getByTestId('user-grants-button'))
      .toHaveText('Hide databases')

    // Add: PUT /users/2/database-grants/replica {"role": "viewer"}.
    await panel.getByTestId('user-grant-database').fill('replica')
    await panel.getByTestId('user-grant-new-role').selectOption('viewer')
    await panel.getByTestId('user-grant-add').click()
    await expect.poll(() => grantRows(panel)).toEqual(['primary=operator', 'replica=viewer'])
    await expect(panel.getByTestId('user-grant-database')).toHaveValue('')
    expect(calls.filter(c => c.method === 'PUT')).toEqual([
      { method: 'PUT', path: '/api/v1/users/2/database-grants/replica', body: { role: 'viewer' } },
    ])

    // Change: the role select PUTs the same database with the new role.
    await panel.getByTestId('user-grant-row').filter({ hasText: 'primary' })
      .getByTestId('user-grant-role').selectOption('viewer')
    await expect.poll(() => grantRows(panel)).toEqual(['primary=viewer', 'replica=viewer'])
    expect(calls.filter(c => c.method === 'PUT').at(-1)).toEqual(
      { method: 'PUT', path: '/api/v1/users/2/database-grants/primary', body: { role: 'viewer' } })

    // Remove: DELETE the grant; the list reloads without it.
    await panel.getByTestId('user-grant-row').filter({ hasText: 'replica' })
      .getByTestId('user-grant-remove').click()
    await expect.poll(() => grantRows(panel)).toEqual(['primary=viewer'])
    expect(calls.filter(c => c.method === 'DELETE')).toEqual([
      { method: 'DELETE', path: '/api/v1/users/2/database-grants/replica', body: null },
    ])
  })

test('no grants: the user keeps their role on every database', async ({ page }) => {
  await routeUsers(page, new Map())
  const panel = await openPanel(page, 'reader@test.com')
  await expect(panel.getByTestId('user-grants-notice'))
    .toHaveText('No grants: viewer on every database.')
  await expect(panel.getByTestId('user-grant-row')).toHaveCount(0)
})

test('removing the last grant returns the user to their role everywhere', async ({ page }) => {
  // State transition: scoped -> unscoped.
  const grants = new Map<number, Grant[]>([[2, [{ database: 'replica', role: 'operator' }]]])
  await routeUsers(page, grants)
  const panel = await openPanel(page, 'ops@test.com')
  await expect(panel.getByTestId('user-grants-notice')).toHaveText(SCOPED)
  const remove = panel.getByTestId('user-grant-remove')
  await expect(remove).toHaveAttribute('title',
    'Removing the last grant returns the user to their role on every database')
  await remove.click()
  await expect(panel.getByTestId('user-grants-notice'))
    .toHaveText('No grants: operator on every database.')
  await expect(panel.getByTestId('user-grant-row')).toHaveCount(0)
})

test('grants saved but not enforced say so', async ({ page }) => {
  const grants = new Map<number, Grant[]>([[2, [{ database: 'primary', role: 'operator' }]]])
  await routeUsers(page, grants, { enforced: false })
  const panel = await openPanel(page, 'ops@test.com')
  await expect(panel.getByTestId('user-grants-notice')).toHaveText(NOT_ENFORCED)
  await expect.poll(() => grantRows(panel)).toEqual(['primary=operator'])
})

test('a grant on an unmonitored database shows the server message and keeps the input',
  async ({ page }) => {
    const grants = new Map<number, Grant[]>()
    const calls: GrantCall[] = []
    await routeUsers(page, grants, { calls })
    const panel = await openPanel(page, 'ops@test.com')
    await panel.getByTestId('user-grant-database').fill('billing')
    await panel.getByTestId('user-grant-add').click()
    await expect(panel.getByTestId('user-grants-error'))
      .toHaveText('unknown database: a grant must name a database pg_sage monitors')
    await expect(panel.getByTestId('user-grant-database')).toHaveValue('billing')
    await expect(panel.getByTestId('user-grant-row')).toHaveCount(0)
    expect(grants.get(2) ?? []).toEqual([])
    expect(calls.filter(c => c.method === 'PUT')).toHaveLength(1)
  })

test('an empty database name sends nothing; admins have no Databases panel',
  async ({ page }) => {
    const calls: GrantCall[] = []
    await routeUsers(page, new Map(), { calls })
    const panel = await openPanel(page, 'ops@test.com')
    await panel.getByTestId('user-grant-database').fill('   ')
    await panel.getByTestId('user-grant-add').click()
    await expect(panel.getByTestId('user-grants-error')).toHaveCount(0)
    expect(calls.filter(c => c.method !== 'GET')).toEqual([])
    await expect(userRowFor(page, 'admin@test.com').getByTestId('user-grants-button'))
      .toHaveCount(0)
  })
