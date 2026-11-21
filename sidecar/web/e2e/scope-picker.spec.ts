// scope-picker.spec.ts (ux-10b): the header scope combobox over a
// 500-database fleet. It searches the fleet list on the server (q, tag,
// 50 a page), selects with the keyboard, offers tag scopes that the Fleet
// page reads and other pages decline, and the home page renders fleet
// summaries instead of one block per database.
// Register routes AFTER mockAllAPIs: Playwright matches the newest first.
// No concurrency tests: the request races are covered in vitest.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

const name = (i: number) => `db-${String(i).padStart(4, '0')}`
const names = Array.from({ length: 500 }, (_, i) => name(i))
names[120] = 'payments'
names[121] = 'payroll'
const prod = new Set(['payments', name(7)])

const fleetRow = (n: string) => ({ name: n, trust_level: 'advisory',
  status: { connected: true, error: null, health_score: 90, trust_level: 'advisory',
    findings_critical: 0, findings_warning: 0 } })

async function routeFleet(page: Page) {
  await page.route('**/api/v1/databases**', route => {
    const url = new URL(route.request().url())
    if (url.pathname.endsWith('/managed')) {
      return route.fulfill({ json: { databases: names.map((n, i) => ({ id: i + 1, name: n,
        host: `${n}.example.com`, port: 5432, database_name: 'app', trust_level: 'advisory',
        execution_mode: 'approval', enabled: true,
        tags: prod.has(n) ? { tier: 'prod' } : {} })) } })
    }
    const q = url.searchParams.get('q') || ''
    const tag = url.searchParams.get('tag')
    if (!url.searchParams.has('limit')) {
      return route.fulfill({ json: { summary: { total_databases: 500, healthy: 500 },
        databases: names.map(fleetRow) } })
    }
    const hits = names.filter(n => n.includes(q) && (!tag || prod.has(n)))
    const start = Number(url.searchParams.get('cursor') || 0)
    const rows = hits.slice(start, start + 50)
    return route.fulfill({ json: { summary: {}, total: hits.length, total_capped: false,
      next_cursor: start + 50 < hits.length ? String(start + 50) : '',
      databases: rows.map(fleetRow) } })
  })
}

async function routeHome(page: Page) {
  await page.route('**/api/v1/first-look**', route => route.fulfill({ json: {
    databases: names.map(n => ({ database: n, report: { database: n, duration_ms: 2,
      relations: 3, checks: [], items: [{ rule: 'r', severity: 'warning', object: 'o',
        title: `Finding on ${n}`, evidence: [] }] } })) } }))
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeFleet(page)
})

test('typing finds a database on the server and Enter selects it', async ({ page }) => {
  await page.goto('/#/cases')
  const scope = page.getByRole('combobox', { name: 'Database scope' })
  await expect(scope).toHaveValue('All databases')
  await scope.click()
  const search = page.waitForRequest(r => r.url().includes('q=pay'))
  await scope.pressSequentially('pay')
  await search
  const list = page.getByRole('listbox')
  await expect(list.getByRole('option')).toHaveText(['All databases', 'payments', 'payroll'])
  await scope.press('ArrowDown')
  await scope.press('ArrowDown')
  await scope.press('Enter')
  await expect(scope).toHaveValue('payments')
  await expect(page.getByRole('listbox')).toHaveCount(0)
})

test('the list pages at 50 and loads more', async ({ page }) => {
  await page.goto('/#/cases')
  const scope = page.getByRole('combobox', { name: 'Database scope' })
  await scope.click()
  await scope.pressSequentially('db-')
  const list = page.getByRole('listbox')
  await expect(list.getByText('50 of 498 shown')).toBeVisible()
  await list.getByRole('option', { name: 'Load more' }).click()
  await expect(list.getByText('100 of 498 shown')).toBeVisible()
})

test('a tag scope filters Fleet and other pages ask for one database', async ({ page }) => {
  await page.goto('/#/manage-databases')
  const scope = page.getByRole('combobox', { name: 'Database scope' })
  await scope.click()
  await scope.pressSequentially('tag:tier=prod')
  await page.getByRole('option', { name: 'All databases tagged tier=prod' }).click()
  await expect(page).toHaveURL(/#\/manage-databases\?tag=tier%3Dprod$/)
  await expect(scope).toHaveValue('tag:tier=prod')
  await page.evaluate(() => { window.location.hash = '#/value?tag=tier%3Dprod' })
  await expect(page.getByTestId('needs-one-database')).toContainText(
    'This page needs one database')
  await page.getByRole('button', { name: 'Show all databases' }).click()
  await expect(page.getByTestId('needs-one-database')).toHaveCount(0)
  await expect(scope).toHaveValue('All databases')
})

test('a link to a database the user cannot see falls back to all', async ({ page }) => {
  await page.goto('/#/cases?database=ghost')
  await expect(page.getByTestId('scope-notice')).toContainText(
    'ghost is not a database you can see. Showing all databases.')
  await expect(page.getByRole('combobox', { name: 'Database scope' }))
    .toHaveValue('All databases')
})

test('the home page summarizes 500 databases', async ({ page }) => {
  await routeHome(page)
  await page.goto('/#/')
  const summary = page.getByTestId('first-look-summary')
  await expect(summary).toContainText('Warning: 500')
  await expect(summary.getByTestId('first-look-worst-row')).toHaveCount(10)
  await expect(summary).toContainText('490 more')
  await expect(page.getByText('Finding on db-0000')).toHaveCount(0)
})
