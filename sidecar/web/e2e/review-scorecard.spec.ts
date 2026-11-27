// review-scorecard.spec.ts (review-05b): the scorecard and the model gate
// card on the Review page and, compact, on the Value page, over a mocked
// API whose scorecard copies review-05a's Go handler golden
// (e2e/mocks/scorecard.ts). No concurrency tests: a read-only render.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs, mockAuthMe } from './fixtures'
import { reviewGrade } from './mocks/review'
import { reviewScorecard } from './mocks/scorecard'

const scorecardPath = '**/api/v1/databases/primary/review/scorecard**'

async function routeScorecard(page: Page, status: number, body: unknown) {
  await page.route(scorecardPath, route => route.fulfill({ status, json: body }))
}

async function asRole(page: Page, role: string) {
  await page.route('**/api/v1/auth/me', route =>
    route.fulfill({ json: { ...mockAuthMe, role } }))
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.route('**/api/v1/databases/primary/review/grade', route =>
    route.fulfill({ json: { ...reviewGrade(), viewed_at: '2026-11-16T09:00:00Z' } }))
})

test('the Review page shows the scorecard by origin and the gate card', async ({ page }) => {
  await routeScorecard(page, 200, reviewScorecard())
  await page.goto('/#/review?db=primary')
  const card = page.getByTestId('review-scorecard')
  await expect(card.getByTestId('scorecard-row-model').getByTestId('cell-improved'))
    .toContainText('45 of 50')
  await expect(card.getByTestId('gate-line')).toHaveText(
    'Model gate: 50 of 50 reviews; model 90% improved vs generators 100%; reverts 0%')
  await expect(card.getByRole('link', { name: 'Approve in the Inbox' }))
    .toHaveAttribute('href', '#/')
  await expect(page.getByTestId('grade-card')).toBeVisible()
})

test('a viewer sees the pending gate but no approval link', async ({ page }) => {
  await asRole(page, 'viewer')
  await routeScorecard(page, 200, reviewScorecard())
  await page.goto('/#/review?db=primary')
  await expect(page.getByTestId('gate-verdict')).toContainText("Waiting for an admin's approval")
  await expect(page.getByRole('link', { name: 'Approve in the Inbox' })).toHaveCount(0)
})

test('the approval link opens the Inbox', async ({ page }) => {
  await routeScorecard(page, 200, reviewScorecard())
  await page.goto('/#/review?db=primary')
  await page.getByRole('link', { name: 'Approve in the Inbox' }).click()
  await expect(page).toHaveURL(/#\/$/)
})

test('a granted gate shows no approval link', async ({ page }) => {
  await routeScorecard(page, 200, reviewScorecard('granted'))
  await page.goto('/#/review?db=primary')
  await expect(page.getByTestId('gate-verdict')).toContainText('Granted by an admin')
  await expect(page.getByRole('link', { name: 'Approve in the Inbox' })).toHaveCount(0)
})

test('the Value page shows the compact scorecard of the selected database', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('pg_sage_db', 'primary'))
  await routeScorecard(page, 200, reviewScorecard())
  await page.goto('/#/value')
  const card = page.getByTestId('review-scorecard')
  await expect(card.getByTestId('gate-line')).toContainText('Model gate: 50 of 50 reviews')
  await expect(card.getByRole('link', { name: 'Open the review' }))
    .toHaveAttribute('href', '#/review?db=primary')
  await expect(card.getByTestId('cell-accepted')).toHaveCount(0)
})

test('a scorecard 500 shows its error and the Value page still renders', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('pg_sage_db', 'primary'))
  await routeScorecard(page, 500, { error: 'the review scorecard of primary could not be read' })
  await page.goto('/#/value')
  await expect(page.getByTestId('review-scorecard').getByRole('alert'))
    .toContainText('the review scorecard of primary could not be read')
  await expect(page.getByRole('heading', { name: 'Value delivered' })).toBeVisible()
})

test('the scorecard fits 375 px with no horizontal page scroll', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 })
  await routeScorecard(page, 200, reviewScorecard())
  await page.goto('/#/review?db=primary')
  await expect(page.getByTestId('gate-line')).toBeVisible()
  const overflow = await page.evaluate(() =>
    document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})
