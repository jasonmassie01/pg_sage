import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

// cases-12: an Ask Sage citation of an investigation resolves through the
// investigation's own record (GET /api/v1/databases/{db}/investigations/
// {id}), read once, never through the database's newest 100.

const INV = '3f2a9c1e-7b4d-4e8a-9c2f-1a2b3c4d5e6f'
const lockCase = {
  id: 'incident:primary:lock_contention:1', source_type: 'incident',
  source_ids: ['1'], database_name: 'primary', title: 'Lock storm on primary',
  severity: 'critical', state: 'open', evidence: [],
}
const otherCase = {
  id: 'finding:primary:index', source_type: 'finding', source_ids: ['2'],
  database_name: 'primary', title: 'Missing index on primary', severity: 'warning',
  state: 'open', evidence: [],
}
// The newest 100 investigations of the database, none of them the cited one.
const newest100 = Array.from({ length: 100 }, (_, i) => ({
  id: `00000000-0000-4000-8000-${String(i).padStart(12, '0')}`,
  case_id: `incident:primary:other:${i}`, database: 'primary', state: 'concluded',
}))

// mockFocus returns the API requests the page made and those that were
// answered: the dev server runs React StrictMode, whose doubled effects
// abort a first read, so "read once" counts answered reads.
async function mockFocus(page: Page): Promise<{ sent: string[], answered: string[] }> {
  await mockAllAPIs(page)
  await page.route('**/api/v1/cases**', route => route.fulfill({
    json: { cases: [lockCase, otherCase] } }))
  await page.route('**/api/v1/investigations**', route => route.fulfill({
    json: { items: newest100 } }))
  await page.route(`**/api/v1/databases/primary/investigations/${INV}`, route =>
    route.fulfill({ json: { database: 'primary', investigation: {
      id: INV, case_id: lockCase.id, state: 'concluded', trigger_kind: 'lock_blocking',
      summary: {},
    } } }))
  const sent: string[] = []
  const answered: string[] = []
  const apiPath = (raw: string) => {
    const url = new URL(raw)
    return url.pathname.startsWith('/api/') ? url.pathname + url.search : null
  }
  page.on('request', req => { const p = apiPath(req.url()); if (p) sent.push(p) })
  page.on('requestfinished', req => { const p = apiPath(req.url()); if (p) answered.push(p) })
  return { sent, answered }
}

const listReads = (paths: string[]) => paths.filter(p =>
  p.startsWith('/api/v1/investigations?') && !p.includes('case_id='))
const detailReads = (paths: string[]) => paths.filter(p =>
  p.startsWith('/api/v1/databases/') && p.includes('/investigations/'))

test('a cited investigation older than the newest 100 opens its case', async ({ page }) => {
  const { sent, answered } = await mockFocus(page)
  await page.goto(`/#/cases?investigation=${INV}&database=primary`)
  await expect(page.getByRole('article')).toHaveCount(1)
  await expect(page.getByRole('heading', { name: 'Lock storm on primary' })).toBeVisible()
  await expect(page.getByTestId('cases-focus-missing')).toHaveCount(0)
  const detail = `/api/v1/databases/primary/investigations/${INV}`
  expect([...new Set(detailReads(sent))]).toEqual([detail])
  expect(detailReads(answered)).toEqual([detail])
  expect(listReads(sent)).toEqual([])
})

test('a hostile investigation id makes no request and says so', async ({ page }) => {
  const { sent } = await mockFocus(page)
  await page.goto('/#/cases?investigation=..%2F..%2Fx&database=primary')
  await expect(page.getByTestId('cases-focus-missing'))
    .toContainText('investigation ../../x')
  await expect(page.getByRole('article')).toHaveCount(0)
  expect(detailReads(sent)).toEqual([])
  expect(listReads(sent)).toEqual([])
})

test('with no database known the page asks for one and reads nothing', async ({ page }) => {
  const { sent } = await mockFocus(page)
  await page.goto(`/#/cases?investigation=${INV}`)
  await expect(page.getByTestId('cases-focus-missing'))
    .toContainText('Choose the database this investigation belongs to')
  expect(detailReads(sent)).toEqual([])
  expect(listReads(sent)).toEqual([])
})
