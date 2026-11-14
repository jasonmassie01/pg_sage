// ask-from-case.spec.ts (dashboard-13): Ask Sage opened from a case sends
// the question answer-only and labels the answer "Answer only" (mcp-01
// CHECK-03), and a citation [1] lands on Cases with the cited case focused
// (mcp-05 CHECK-03), in the real bundle with real routing and clicks.
// Payloads: e2e/mocks/cases.ts and e2e/mocks/ask.ts.
// No concurrency tests: one page reads its own mocked responses.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { findingCase, routeCases, routeFleet } from './mocks/cases'
import { askAnswer, citation, routeAsk } from './mocks/ask'

const statsCase = findingCase('12', 'orders', 'stale_statistics')
const indexCase = findingCase('13', 'orders', 'unused_index')
const STATS_TITLE = 'Query planning information may be out of date'
const INDEX_TITLE = 'An index may no longer be needed'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeFleet(page)
  await routeCases(page, () => ({ cases: [statsCase, indexCase], total: 2 }))
})

// askAbout opens Ask Sage on orders, types question and sends it.
async function askAbout(page: Page, question: string) {
  await page.goto('/#/ask?database=orders')
  await page.getByTestId('ask-input').fill(question)
  await page.getByTestId('ask-submit').click()
}

test('mcp-01 CHECK-03: Ask from a case sends answer-only and shows "Answer only"',
  async ({ page }) => {
    // The answer echoes read_only only when the page sent it.
    const posts = await routeAsk(page, body => askAnswer(String(body.question),
      { readOnly: body.read_only === true }))
    await page.goto('/#/cases?database=orders')
    const card = page.getByRole('article').filter({ hasText: STATS_TITLE })
    await card.getByRole('link', { name: 'Explain this' }).click()

    await expect(page).toHaveURL(/#\/ask\?case=finding%3Aorders%3Astale_statistics/)
    await expect(page.getByTestId('ask-case-read-only')).toContainText('answer only')
    await expect(page.getByTestId('ask-input')).toHaveValue(/finding 12 in database orders/)
    await page.getByTestId('ask-submit').click()

    const answer = page.getByTestId('ask-answer-101')
    await expect(answer.getByTestId('ask-read-only')).toHaveText('Answer only')
    expect(posts).toHaveLength(1)
    expect(posts[0].postDataJSON()).toMatchObject({ read_only: true })
    expect(new URL(posts[0].url()).pathname).toBe('/api/v1/databases/orders/ask')
  })

test('a question asked outside a case is not answer-only', async ({ page }) => {
  const posts = await routeAsk(page, body => askAnswer(String(body.question),
    { readOnly: body.read_only === true }))
  await askAbout(page, 'Which findings are critical?')
  await expect(page.getByTestId('ask-answer-101')).toBeVisible()
  await expect(page.getByTestId('ask-read-only')).toHaveCount(0)
  expect(posts[0].postDataJSON()).not.toHaveProperty('read_only')
})

test('mcp-05 CHECK-03: citation [1] focuses the cited case', async ({ page }) => {
  await routeAsk(page, body => askAnswer(String(body.question),
    { citations: [citation('E1', 'finding', '12')] }))
  await askAbout(page, 'Which findings are critical?')
  await page.getByTestId('ask-statement').getByRole('link', { name: '[1]' }).click()

  await expect(page).toHaveURL(/#\/cases\?finding=12&database=orders$/)
  const cards = page.getByRole('article')
  await expect(cards).toHaveCount(1)
  await expect(cards.getByRole('heading', { name: STATS_TITLE })).toBeVisible()
  await expect(page.getByRole('heading', { name: INDEX_TITLE })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Back to all cases' })).toBeVisible()
  await expect(page.getByTestId('cases-focus-missing')).toHaveCount(0)
})

test('a citation to a non-numeric finding id opens no case and shows the notice',
  async ({ page }) => {
    await routeAsk(page, body => askAnswer(String(body.question),
      { citations: [citation('E1', 'finding', '12abc')] }))
    await askAbout(page, 'Which findings are critical?')
    await page.getByTestId('ask-statement').getByRole('link', { name: '[1]' }).click()

    await expect(page).toHaveURL(/#\/cases\?finding=12abc&database=orders$/)
    const notice = page.getByTestId('cases-focus-missing')
    await expect(notice).toContainText('No open case holds finding 12abc')
    // "12abc" never matches finding 12 by prefix.
    await expect(page.getByRole('article')).toHaveCount(0)
    await expect(notice.getByRole('link', { name: 'View the finding record' }))
      .toHaveAttribute('href', '/api/v1/findings/12abc?database=orders')
  })

test('an answer with no citations has no links', async ({ page }) => {
  await routeAsk(page, body => askAnswer(String(body.question)))
  await askAbout(page, 'Which findings are critical?')
  const answer = page.getByTestId('ask-answer-101')
  await expect(answer.getByTestId('ask-statement'))
    .toHaveText('The table statistics of public.t12 are out of date.')
  await expect(answer.getByRole('link')).toHaveCount(0)
  await expect(answer.getByTestId('ask-citations')).toHaveCount(0)
})
