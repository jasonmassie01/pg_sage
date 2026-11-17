// review-page.spec.ts (review-01b): the Review page over a mocked API. The
// grade payload copies the Go golden (e2e/mocks/review.ts). No concurrency
// tests: one person reads one page; the API test races two tabs.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { reviewGrade, reviewWithModelPass } from './mocks/review'

const gradePath = '**/api/v1/databases/primary/review/grade'
const viewedPath = '**/api/v1/databases/primary/review/runs/*/viewed'

async function routeGrade(page: Page, status: number, body: unknown) {
  await page.route(gradePath, route => route.fulfill({ status, json: body }))
}

async function routeViewed(page: Page, status = 200) {
  const posts: string[] = []
  await page.route(viewedPath, route => {
    // The real server refuses a POST without a JSON Content-Type (415).
    const type = route.request().headers()['content-type'] || 'none'
    posts.push(`${new URL(route.request().url()).pathname} ${type}`)
    return route.fulfill({ status, json: status === 200
      ? { run_id: 12, viewed_at: '2026-11-16T10:00:00Z', first: true }
      : { error: 'the read receipt could not be recorded' } })
  })
  return posts
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('shows the grade, the eight dimensions, the delta and the report', async ({ page }) => {
  await routeGrade(page, 200, reviewGrade())
  const posts = await routeViewed(page)
  await page.goto('/#/review?db=primary')
  await expect(page.getByTestId('grade-card').getByTestId('letter-badge')).toHaveText('B')
  await expect(page.getByTestId('grade-card')).toContainText('Rubric pg_sage-rubric/1')
  await expect(page.getByTestId('dimension-row')).toHaveCount(8)
  await expect(page.getByTestId('delta-check')).toHaveCount(2)
  await expect(page.getByTestId('model-pass')).toContainText('Model pass: skipped')
  await expect(page.getByTestId('report-finding').first()).toBeVisible()
  await expect.poll(() => posts).toEqual(
    ['/api/v1/databases/primary/review/runs/12/viewed application/json'])
})

test('links the plan of a review with a model pass', async ({ page }) => {
  await routeGrade(page, 200, reviewWithModelPass())
  await routeViewed(page)
  await page.goto('/#/review?db=primary')
  await expect(page.getByTestId('model-pass')).toContainText('Model pass: done by')
  await expect(page.getByTestId('report-hypothesis')).toContainText('Advice')
  await expect(page.getByRole('link', { name: 'Review the plan' }))
    .toHaveAttribute('href', '#/actions?tab=plans&plan=p-77&database=primary')
  await expect(page.locator('[data-testid="review-report"] img')).toHaveCount(0)
})

test('a 500 shows the error with Retry and no grade', async ({ page }) => {
  await routeGrade(page, 500, { error: 'the review grade of primary could not be read' })
  await page.goto('/#/review?db=primary')
  await expect(page.getByRole('alert'))
    .toContainText('the review grade of primary could not be read')
  await expect(page.getByRole('button', { name: 'Retry' })).toBeVisible()
  await expect(page.getByTestId('grade-card')).toHaveCount(0)
})

test('a failed read receipt never blocks the page', async ({ page }) => {
  await routeGrade(page, 200, reviewGrade())
  const posts = await routeViewed(page, 500)
  await page.goto('/#/review?db=primary')
  await expect(page.getByTestId('grade-card')).toBeVisible()
  await expect.poll(() => posts.length, { timeout: 5000 }).toBe(2)
  await expect(page.getByTestId('dimension-row')).toHaveCount(8)
})

test('the eight dimensions fit 375 px with no horizontal scroll', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 })
  await routeGrade(page, 200, reviewGrade())
  await routeViewed(page)
  await page.goto('/#/review?db=primary')
  await expect(page.getByTestId('dimension-row')).toHaveCount(8)
  const overflow = await page.evaluate(() =>
    document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})
