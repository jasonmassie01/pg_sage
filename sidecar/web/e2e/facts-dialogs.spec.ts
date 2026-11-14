// facts-dialogs.spec.ts (dashboard-18): the confirmation before a decision
// that lifts a fact's guard (facts-07 CHECK-03, reports/t1/facts-07.md:155
// MANUAL "check the dialog wording in a browser"). Payloads: e2e/mocks/facts.ts.
// No concurrency tests: one operator decides one fact at a time.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { confirmedFact, routeFacts } from './mocks/facts'

async function openAll(page: Page) {
  await page.goto('/#/facts')
  await page.getByTestId('facts-tab-all').click()
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('facts-07 CHECK-03: revoke asks first, naming the fact and what may act again',
  async ({ page }) => {
    const decisions = await routeFacts(page, [confirmedFact(9)])
    await openAll(page)
    await expect(page.getByTestId('fact-status-9')).toHaveText('confirmed')
    await page.getByTestId('fact-note-9').fill('consumer retired')
    await page.getByTestId('fact-revoke-9').click()
    const dialog = page.getByRole('alertdialog', {
      name: 'Revoke fact #9: slot cdc_orders belongs to debezium?' })
    await expect(dialog).toContainText(
      'It stops binding. pg_sage and operator approvals may then act on cdc_orders again.')
    await expect(dialog.getByTestId('fact-widen-confirm-9')).toHaveText('Revoke')
    // Nothing is sent, and no other decision can start, until the dialog answers.
    expect(decisions).toEqual([])
    await expect(page.getByTestId('fact-expire-9')).toBeDisabled()

    await dialog.getByTestId('fact-widen-confirm-9').click()
    await expect(page.getByTestId('fact-status-9')).toHaveText('rejected')
    expect(decisions).toEqual([{ url: '/api/v1/facts/9/reject?database=orders',
      body: { note: 'consumer retired' } }])
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
  })

test('facts-07 CHECK-03: expire uses its own verb; Cancel sends nothing', async ({ page }) => {
  const decisions = await routeFacts(page, [confirmedFact(9)])
  await openAll(page)
  await page.getByTestId('fact-expire-9').click()
  const dialog = page.getByRole('alertdialog', {
    name: 'Expire fact #9: slot cdc_orders belongs to debezium?' })
  await expect(dialog.getByTestId('fact-widen-confirm-9')).toHaveText('Expire')
  await dialog.getByTestId('fact-widen-cancel-9').click()
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  expect(decisions).toEqual([])
  await expect(page.getByTestId('fact-status-9')).toHaveText('confirmed')

  await page.getByTestId('fact-expire-9').click()
  await page.getByTestId('fact-widen-confirm-9').click()
  await expect(page.getByTestId('fact-status-9')).toHaveText('expired')
  expect(decisions.map(d => d.url)).toEqual(['/api/v1/facts/9/expire?database=orders'])
})

test('confirming a proposal stays one click, with no dialog', async ({ page }) => {
  const decisions = await routeFacts(page, [confirmedFact(4, { status: 'proposed' })])
  await page.goto('/#/facts')
  await page.getByTestId('fact-confirm-4').click()
  await expect(page.getByTestId('fact-card-4')).toHaveCount(0)
  expect(decisions.map(d => d.url)).toEqual(['/api/v1/facts/4/confirm?database=orders'])
  await page.getByTestId('facts-tab-confirmed').click()
  await expect(page.getByTestId('fact-status-4')).toHaveText('confirmed')
})

test('a fact summary containing markup renders as text in the dialog', async ({ page }) => {
  const summary = 'slot <img src=x onerror=alert(1)> belongs to debezium'
  await routeFacts(page, [confirmedFact(9, { summary,
    subject: '<img src=y onerror=alert(2)>', value: { consumer: '<b>debezium</b>' } })])
  await openAll(page)
  await expect(page.getByTestId('fact-card-9')).toContainText('consumer: <b>debezium</b>')
  await page.getByTestId('fact-revoke-9').click()
  const dialog = page.getByRole('alertdialog')
  await expect(dialog.locator('div').first()).toHaveText(`Revoke fact #9: ${summary}?`)
  await expect(dialog).toContainText('may then act on <img src=y onerror=alert(2)> again.')
  await expect(page.locator('img[src="x"], img[src="y"]')).toHaveCount(0)
  await expect(page.locator('b').filter({ hasText: 'debezium' })).toHaveCount(0)
})

test('a refused revoke keeps the fact confirmed and says why', async ({ page }) => {
  await routeFacts(page, [confirmedFact(9)],
    () => ({ status: 409, json: { error: 'conflict', code: 'content_changed' } }))
  await openAll(page)
  await page.getByTestId('fact-revoke-9').click()
  await page.getByTestId('fact-widen-confirm-9').click()
  await expect(page.getByTestId('fact-error-9')).toHaveText('Could not reject fact #9: ' +
    'This fact changed since it was shown. Reload and review it again.')
  await expect(page.getByTestId('fact-status-9')).toHaveText('confirmed')
  await expect(page.getByTestId('fact-revoke-9')).toBeEnabled()
})

test('no facts shows the empty state for the tab', async ({ page }) => {
  await routeFacts(page, [])
  await page.goto('/#/facts')
  await expect(page.getByTestId('facts-empty')).toContainText('No proposed facts.')
  await page.getByTestId('facts-tab-all').click()
  await expect(page.getByTestId('facts-empty')).toContainText('No facts yet.')
})
