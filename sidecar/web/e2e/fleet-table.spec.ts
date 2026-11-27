// fleet-table.spec.ts (ux-10a): the Databases table over 500 registrations
// pages, searches, tag-filters and sorts through the server (the route
// below answers GET /api/v1/databases like the sidecar does), and bulk
// tests the selected rows through the per-row route.
// Register routes AFTER mockAllAPIs: Playwright matches the newest first.
// No concurrency tests: one admin drives one page; the server's paging
// under a reload is tested in internal/api.

import { expect, test, type Page, type Route } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

const name = (i: number) => (i % 4 === 0 ? `pay-${String(i).padStart(3, '0')}`
  : `db-${String(i).padStart(3, '0')}`)

const managed = Array.from({ length: 500 }, (_, i) => ({
  id: i + 1, name: name(i), host: `${name(i)}.example.com`, port: 5432,
  database_name: 'app', trust_level: 'advisory', execution_mode: 'approval', enabled: true,
  tags: { tier: i % 2 === 0 ? 'prod' : 'dev' },
}))

const runtime = managed.map((m, i) => ({ id: m.id, database_id: m.id, name: m.name,
  tags: [`tier=${m.tags.tier}`], status: { connected: true, health_score: 100 - (i % 50),
    findings_open: i % 7, findings_critical: 0, findings_warning: 0, error: null } }))

const lists: string[] = []
const tests: string[] = []

function listPage(url: URL) {
  const q = (url.searchParams.get('q') || '').toLowerCase()
  const tags = url.searchParams.getAll('tag').map(t => t.toLowerCase())
  const rows = runtime.filter(r => (!q || r.name.includes(q) ||
    `${r.name}.example.com`.includes(q)) && tags.every(t => r.tags.includes(t)))
  const health = url.searchParams.get('sort') === 'health'
  const sign = url.searchParams.get('order') === 'desc' ? -1 : 1
  rows.sort((a, b) => sign * (health ? a.status.health_score - b.status.health_score
    : a.name.localeCompare(b.name)) || a.name.localeCompare(b.name))
  const start = Number(url.searchParams.get('cursor') || 0)
  const limit = Number(url.searchParams.get('limit'))
  const next = start + limit < rows.length ? String(start + limit) : ''
  return { mode: 'fleet', summary: { total_databases: 500 }, total: rows.length,
    total_capped: false, next_cursor: next, databases: rows.slice(start, start + limit) }
}

async function routeFleet(page: Page) {
  await page.route('**/api/v1/databases**', (route: Route) => {
    const url = new URL(route.request().url())
    const test = url.pathname.match(/\/managed\/(\d+)\/test$/)
    if (test) {
      tests.push(test[1])
      return route.fulfill({ json: { status: test[1] === '2' ? 'error' : 'ok' } })
    }
    if (url.pathname.endsWith('/managed')) return route.fulfill({ json: { databases: managed } })
    if (!url.searchParams.has('sort')) {
      return route.fulfill({ json: { summary: { total_databases: 500, healthy: 500,
        degraded: 0, total_critical: 0, total_warning: 0, emergency_stopped: false },
      databases: runtime } })
    }
    lists.push(url.search)
    return route.fulfill({ json: listPage(url) })
  })
}

test.beforeEach(async ({ page }) => {
  lists.length = 0
  tests.length = 0
  await mockAllAPIs(page)
  await routeFleet(page)
  await page.goto('/#/manage-databases')
})

const table = (page: Page) => page.getByTestId('databases-table')
const rows = (page: Page) => table(page).getByTestId('db-row')

test('ux-10a: 500 databases page through Load more, 50 at a time', async ({ page }) => {
  await expect(rows(page)).toHaveCount(50)
  await expect(page.getByTestId('db-filter-count')).toContainText('Showing 50 of 500')
  await page.getByTestId('db-load-more').click()
  await expect(rows(page)).toHaveCount(100)
  await expect(rows(page).nth(50)).toContainText('db-067')
  expect(lists[0]).toBe('?sort=name&order=asc&limit=50')
})

test('ux-10a: search and tag chips go to the server, empty state clears', async ({ page }) => {
  await page.getByRole('searchbox', { name: 'Search databases' }).fill('PAY-01')
  await expect(rows(page)).toHaveCount(2)
  await expect(rows(page).first()).toContainText('pay-012')
  const tag = page.getByRole('textbox', { name: 'Add tag filter' })
  await tag.fill('tier=dev')
  await tag.press('Enter')
  await expect(page.getByTestId('db-filter-empty')).toContainText(
    'No databases match; clear filters')
  expect(lists.at(-1)).toContain('tag=tier%3Ddev')
  await page.getByTestId('db-filter-clear').click()
  await expect(rows(page)).toHaveCount(50)
  await expect(page.getByTestId('db-tag-chips')).toHaveCount(0)
})

test('ux-10a: a header sorts on the server and says so in aria-sort', async ({ page }) => {
  const health = page.getByRole('columnheader', { name: /Health/ })
  await expect(health).toHaveAttribute('aria-sort', 'none')
  await page.getByTestId('db-sort-Health').click()
  await expect(health).toHaveAttribute('aria-sort', 'ascending')
  await expect(rows(page).first()).toContainText('db-049')
  await page.getByTestId('db-sort-Health').click()
  await expect(health).toHaveAttribute('aria-sort', 'descending')
  await expect(rows(page).first()).toContainText('db-050')
})

test('ux-10a: bulk test shows each result; a new filter clears the selection',
  async ({ page }) => {
    await page.getByRole('checkbox', { name: 'Select db-001' }).check()
    await page.getByRole('checkbox', { name: 'Select db-002' }).check()
    await expect(page.getByTestId('db-bulk-count')).toHaveText('2 selected')
    await page.getByTestId('db-bulk-test').click()
    await expect(page.getByTestId('db-bulk-summary')).toHaveText(
      'Tested 2: 1 connected, 1 failed')
    expect(tests.sort()).toEqual(['2', '3'])
    await expect(rows(page).filter({ hasText: 'db-001' }).getByTestId('db-registration'))
      .toHaveText('test: error')
    await expect(rows(page).filter({ hasText: 'db-002' }).getByTestId('db-registration'))
      .toHaveText('test: connected')
    await page.getByRole('searchbox', { name: 'Search databases' }).fill('db-00')
    await expect(page.getByTestId('db-bulk-count')).toHaveCount(0)
  })
