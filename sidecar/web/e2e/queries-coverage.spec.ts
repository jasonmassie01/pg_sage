// queries-coverage.spec.ts (evid-01b CHECK-09): the workload explorer's
// one-line coverage notice in the real bundle with real routing. The
// payload copies the Go handler test's golden (e2e/mocks/queries.ts).
// No concurrency tests: one page reads its own mocked responses.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { queriesCoverage, routeQueriesCoverage } from './mocks/queries'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('evid-01b CHECK-09: the notice states the long history coverage', async ({ page }) => {
  const urls = await routeQueriesCoverage(page, () => queriesCoverage())
  await page.goto('/#/queries')
  await expect(page.getByTestId('coverage-notice')).toContainText(
    'Long history: 87% of execution time covered, 3 epoch breaks in 28 days')
  expect(urls.some(u => u.startsWith('/api/v1/queries/coverage?database='))).toBe(true)
})

test('frequent statistics resets say the long window is mostly breaks', async ({ page }) => {
  await routeQueriesCoverage(page, () => queriesCoverage({ status: 'mostly_breaks',
    covered_exec_share: 0.31, epoch_breaks: 240 }))
  await page.goto('/#/queries')
  await expect(page.getByTestId('coverage-notice')).toHaveText(/Statistics reset often here/)
})

test('a well covered history says nothing', async ({ page }) => {
  const urls = await routeQueriesCoverage(page,
    () => queriesCoverage({ covered_exec_share: 0.97 }))
  await page.goto('/#/queries')
  await expect(page.getByText('Top statements, last hour')).toBeVisible()
  await expect.poll(() => urls.length).toBeGreaterThan(0)
  await expect(page.getByTestId('coverage-notice')).toHaveCount(0)
})
