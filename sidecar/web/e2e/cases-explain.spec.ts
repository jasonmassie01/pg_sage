// cases-explain.spec.ts (dashboard-13): the Explain panel opened from a
// query case (tuning-10 CHECK-23) and one click from a query-hint case to
// its query page with history and plan (market-01 CHECK-04), in the real
// bundle with real routing and clicks. Payloads: e2e/mocks/cases.ts and
// e2e/mocks/queries.ts.
// No concurrency tests: one page reads its own mocked responses.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { HINT_QUERYID, queryHintCase, routeCases, routeFleet } from './mocks/cases'
import {
  explainFailure, explainRulesOnly, routeExplain, routeQueryDetail,
} from './mocks/queries'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeFleet(page)
})

// openEvidence shows the cases and opens the card's technical evidence.
async function openEvidence(page: Page, cases: unknown[]) {
  await routeCases(page, () => ({ cases, total: cases.length }))
  await page.goto('/#/cases')
  const card = page.getByRole('article')
  await expect(card).toHaveCount(cases.length)
  await card.getByTestId('case-technical-details').locator('summary').click()
  return card.getByLabel('Case evidence')
}

test('tuning-10 CHECK-23: the Explain panel opens from a query case with rule entries',
  async ({ page }) => {
    const requests = await routeExplain(page, () => ({ status: 200, json: explainRulesOnly }))
    const evidence = await openEvidence(page, [queryHintCase()])
    await evidence.getByRole('button', { name: 'Explain this query' }).click()

    const panel = page.getByTestId('explain-panel')
    await expect(panel).toContainText(`Explain of query ${HINT_QUERYID}`)
    const reason = panel.getByTestId('explain-slow-because').getByRole('listitem')
    await expect(reason).toHaveText('sort spilled 8.2 MB to disk' + 'rules')
    await expect(panel.getByTestId('explain-recommendations').getByRole('listitem'))
      .toHaveText('raise work_mem for this statement' + 'rules')
    await expect(panel.getByTestId('explain-plan-source'))
      .toHaveText('Plan: fresh EXPLAIN ANALYZE')
    expect(requests).toHaveLength(1)
    expect(new URL(requests[0].url()).search).toBe('?database=orders')
    // The id travels as the exact decimal string, never a rounded number.
    expect(requests[0].postDataJSON()).toEqual({ query_id: HINT_QUERYID })
  })

test('a 500 from /api/v1/explain shows the panel error with Retry', async ({ page }) => {
  const requests = await routeExplain(page, n => (n === 1
    ? { status: 500, json: explainFailure } : { status: 200, json: explainRulesOnly }))
  const evidence = await openEvidence(page, [queryHintCase()])
  await evidence.getByRole('button', { name: 'Explain this query' }).click()

  const panel = page.getByTestId('explain-panel')
  await expect(panel.getByRole('alert')).toContainText(explainFailure.error)
  await expect(panel.getByTestId('explain-slow-because')).toHaveCount(0)
  await panel.getByRole('button', { name: 'Retry' }).click()
  await expect(panel.getByTestId('explain-slow-because'))
    .toContainText('sort spilled 8.2 MB to disk')
  await expect(panel.getByRole('alert')).toHaveCount(0)
  expect(requests).toHaveLength(2)
  expect(requests[1].postDataJSON()).toEqual({ query_id: HINT_QUERYID })
})

test('market-01 CHECK-04: one click from a query-hint case opens its query page',
  async ({ page }) => {
    const urls = await routeQueryDetail(page)
    const evidence = await openEvidence(page, [queryHintCase()])
    await evidence.getByTestId('case-query-link').click()

    await expect(page).toHaveURL(
      new RegExp(`#/query\\?queryid=${HINT_QUERYID}&database=orders$`))
    await expect(page.getByRole('heading', { name: `Query ${HINT_QUERYID} on orders` }))
      .toBeVisible()
    const history = page.getByTestId('query-history')
    await expect(history.locator('tbody tr')).toHaveCount(2)
    await expect(history.getByTestId('plan-change')).toContainText(': p1 → p2')
    const plan = page.getByTestId('query-plan')
    await expect(plan).toContainText('Sort → Seq Scan')
    await expect(plan).toContainText('Source auto_explain, cost 42.5, fingerprint p2')
    await expect(page.getByTestId('query-hints')).toContainText('Set(work_mem "64MB")')
    expect(urls.some(u => u.startsWith(
      `/api/v1/queries/${HINT_QUERYID}?database=orders`))).toBe(true)
  })

test('a non-numeric query id gives no query link and no Explain button', async ({ page }) => {
  const requests = await routeExplain(page, () => ({ status: 200, json: explainRulesOnly }))
  const evidence = await openEvidence(page, [queryHintCase('12abc')])
  await expect(evidence).toContainText('Hint Set(work_mem "64MB") is active')
  await expect(evidence.getByTestId('case-query-link')).toHaveCount(0)
  await expect(evidence.getByRole('button', { name: 'Explain this query' })).toHaveCount(0)
  expect(requests).toHaveLength(0)
})
