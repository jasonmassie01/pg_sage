// llm-card.spec.ts (dashboard-15): the "Latest briefing" card on the
// Snapshot page shows the stored LLM briefing with its open counts and
// delivery outcome (llm-06b CHECK-05), "No briefing yet" with the server's
// reason on a 404, and an error with Retry on anything else.
// Payloads: e2e/mocks/value.ts.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { NO_BRIEFING, briefing, routeBriefing } from './mocks/value'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('llm-06b CHECK-05: the card shows the latest LLM briefing and its delivery',
  async ({ page }) => {
    await routeBriefing(page, () => ({
      json: { briefings: [briefing], without_briefing: ['reporting'], unavailable: [] },
    }))
    await page.goto('/#/advanced')
    const card = page.getByTestId('briefing-card')
    await expect(card.getByRole('heading', { name: 'Latest briefing' })).toBeVisible()
    const row = card.getByTestId('briefing-orders')
    await expect(row).toContainText('LLM summary')
    await expect(row).toContainText('3 open (1 critical, 2 warning)')
    await expect(row.getByTestId('briefing-delivery-orders'))
      .toHaveText('pagerduty: error (HTTP 503) · slack: sent (HTTP 200)')
    await expect(row.getByTestId('briefing-text-orders')).toBeHidden()
    await row.getByText('Read the briefing').click()
    await expect(row.getByTestId('briefing-text-orders')).toHaveText(briefing.content_text)
    await expect(card.getByTestId('briefing-missing')).toHaveText('No briefing yet: reporting')
    await expect(card.getByTestId('briefing-unavailable')).toHaveCount(0)
  })

test('a structured briefing never dispatched says so', async ({ page }) => {
  await routeBriefing(page, () => ({ json: { briefings: [{ ...briefing, llm_used: false,
    delivery_status: {} }], without_briefing: [], unavailable: [] } }))
  await page.goto('/#/advanced')
  const row = page.getByTestId('briefing-orders')
  await expect(row).toContainText('Structured')
  await expect(row).not.toContainText('LLM summary')
  await expect(row.getByTestId('briefing-delivery-orders')).toHaveText('not dispatched')
})

test('a 404 shows "No briefing yet" with the server reason', async ({ page }) => {
  await routeBriefing(page, () => ({ status: 404, json: { error: NO_BRIEFING } }))
  await page.goto('/#/advanced')
  const empty = page.getByTestId('briefing-card').getByTestId('briefing-empty')
  await expect(empty).toContainText('No briefing yet')
  await expect(empty).toContainText('an empty briefing.schedule turns briefings off')
  await expect(empty).not.toContainText('(404)')
  await expect(page.getByTestId('briefing-error')).toHaveCount(0)
})

test('a 503 shows the error with Retry, not "No briefing yet"', async ({ page }) => {
  let healthy = false
  await routeBriefing(page, () => healthy
    ? { json: { briefings: [briefing], without_briefing: [], unavailable: [] } }
    : { status: 503, json: { error: 'no connected database' } })
  await page.goto('/#/advanced')
  const error = page.getByTestId('briefing-error')
  await expect(error).toContainText('no connected database (503)')
  await expect(page.getByTestId('briefing-empty')).toHaveCount(0)
  healthy = true
  await error.getByRole('button', { name: 'Retry' }).click()
  await expect(page.getByTestId('briefing-orders')).toContainText('3 open')
})
