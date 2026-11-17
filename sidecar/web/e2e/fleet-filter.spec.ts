// fleet-filter.spec.ts (ux-05e, wins 14 and 19): finding one database among
// 500 on the Databases page and in the header scope picker, and the
// external agent request links to the investigation and its approval.
// Register routes AFTER mockAllAPIs: Playwright matches the newest first.
// No concurrency tests: one admin drives one page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

const name = (i: number) => `db-${String(i).padStart(4, '0')}`

// managed copies the GET /api/v1/databases/managed row shape.
const managed = Array.from({ length: 500 }, (_, i) => ({
  id: i + 1, name: name(i), host: `${name(i)}.example.com`, port: 5432,
  database_name: 'app', trust_level: 'advisory', execution_mode: 'approval', enabled: true,
  tags: i === 7 ? { env: 'orders-prod' } : {},
}))
managed[123] = { ...managed[123], name: 'orders' }

// fleet copies the GET /api/v1/databases row shape the header picker reads.
const fleet = managed.map(m => ({ name: m.name, trust_level: 'advisory',
  status: { connected: true, error: null, health_score: 90, findings_critical: 0,
    findings_warning: 0 } }))

async function routeFleet(page: Page) {
  await page.route('**/api/v1/databases**', route => {
    if (route.request().url().includes('/managed')) {
      return route.fulfill({ json: { databases: managed } })
    }
    return route.fulfill({ json: { summary: { total_databases: 500, healthy: 500,
      degraded: 0, total_critical: 0, total_warning: 0, emergency_stopped: false },
    databases: fleet } })
  })
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeFleet(page)
})

test('ux-05e: the Databases page filters 500 registrations', async ({ page }) => {
  await page.goto('/#/manage-databases')
  const rows = page.getByTestId('db-row')
  await expect(page.getByTestId('db-filter-count')).toContainText('of 500')
  const box = page.getByRole('searchbox', { name: 'Filter databases' })
  await box.fill('ord')
  // The name and the tag both match: orders, and db-0007 tagged orders-prod.
  await expect(rows).toHaveCount(2)
  await expect(rows.first()).toContainText('db-0007')
  await expect(rows.nth(1)).toContainText('orders')
  await box.fill('.*')
  await expect(rows).toHaveCount(0)
  await expect(page.getByTestId('db-filter-empty')).toContainText('No database matches')
  await box.fill('')
  await expect(page.getByTestId('db-filter-count')).toContainText('of 500')
})

test('ux-05e: the scope picker finds a database in at most its name length',
  async ({ page }) => {
    await page.goto('/#/')
    const scope = page.getByRole('combobox', { name: 'Database scope' })
    await expect(scope).toHaveValue('all')
    const find = page.getByLabel('Find database')
    await find.click()
    await find.pressSequentially('orders')
    await expect(scope).toHaveValue('orders')
    await expect(find).toHaveValue('')
    // Clearing a filter keeps the selected scope.
    await find.pressSequentially('db-00')
    await expect(scope).toHaveValue('orders')
    await find.fill('')
    await expect(scope).toHaveValue('orders')
  })

test('ux-05e: a request row links its investigation and approval', async ({ page }) => {
  const inv = '44444444-4444-4444-8444-444444444444'
  await page.route('**/api/v1/specialist-requests**', route => route.fulfill({ json: {
    items: [{ id: 'r1', kind: 'remediation', identity_name: 'AWS DevOps Agent',
      actor: 'agent:aws:t-1', transport: 'http', database: 'orders',
      investigation_id: inv, created: false,
      remediation_id: 'cancel_backend.55555555-5555-4555-8555-555555555555',
      verdict: 'queued_for_approval', queue_id: 4242, outbound: 'none',
      created_at: '2026-11-16T09:00:00Z' }] } }))
  await page.goto('/#/mcp-tokens')
  const row = page.getByTestId('specialist-request-r1')
  await expect(row.getByTestId('specialist-request-investigation-r1')).toHaveAttribute(
    'href', `#/cases?investigation=${inv}&database=orders`)
  await row.getByTestId('specialist-request-approval-r1').click()
  await expect(page).toHaveURL(/#\/actions\?tab=pending&queue=4242&database=orders$/)
})
