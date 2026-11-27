import { expect, test } from '@playwright/test'
import { mockAllAPIs, mockCases } from './fixtures'

// rules-12b: the Cases page's "Issue source" select asks the server for
// one subsystem's findings and keeps the choice in the hash.

const optimizerCase = {
  ...mockCases.cases[0],
  id: 'finding:primary:missing_index:table:public.orders',
  case_id: 'case-opt', title: 'Missing index on orders',
}
const rulesCase = {
  ...mockCases.cases[0],
  id: 'finding:primary:unused_index:index:public.idx',
  case_id: 'case-rules', title: 'Unused index idx',
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.route('**/api/v1/cases**', route => {
    const source = new URL(route.request().url()).searchParams.get('finding_source')
    return route.fulfill({ json: {
      cases: source === 'optimizer' ? [optimizerCase] : [optimizerCase, rulesCase],
      total: source === 'optimizer' ? 1 : 2, unavailable: [], truncated: {},
      finding_sources: ['forecaster', 'optimizer', 'rules'],
      ...(source ? { filtered: { finding_source: source } } : {}),
    } })
  })
})

test('choosing a finding source filters on the server and survives a reload',
  async ({ page }) => {
    await page.goto('/#/cases')
    await expect(page.getByRole('heading', { name: 'Unused index idx' })).toBeVisible()

    const filtered = page.waitForRequest(req =>
      req.url().includes('/api/v1/cases') &&
      new URL(req.url()).searchParams.get('finding_source') === 'optimizer')
    await page.getByLabel('Issue source').selectOption('optimizer')
    await filtered

    await expect(page.getByRole('heading', { name: 'Missing index on orders' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Unused index idx' })).toHaveCount(0)
    await expect(page).toHaveURL(/finding_source=optimizer/)

    await page.reload()
    await expect(page.getByLabel('Issue source')).toHaveValue('optimizer')
    await expect(page.getByRole('heading', { name: 'Missing index on orders' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Unused index idx' })).toHaveCount(0)

    await page.getByLabel('Issue source').selectOption('')
    await expect(page.getByRole('heading', { name: 'Unused index idx' })).toBeVisible()
    await expect(page).not.toHaveURL(/finding_source/)
  })
