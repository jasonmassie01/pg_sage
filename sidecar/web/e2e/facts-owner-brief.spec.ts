// facts-owner-brief.spec.ts (evid-03b): the owner brief form on the Facts
// page (declare the brief, apply a measured peak-window suggestion) and
// the "Do not touch" fact in the declare form. Payloads: e2e/mocks/facts.ts.
// No concurrency tests: one operator edits one form at a time.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { ownerBrief, peakSuggestion, routeDeclarations } from './mocks/facts'

async function openFacts(page: Page) {
  await page.goto('/#/facts')
  await page.getByRole('combobox', { name: 'Database scope' }).selectOption('primary')
  await expect(page.getByTestId('owner-brief-form')).toBeVisible()
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('declares the owner brief with the applied peak-window suggestion',
  async ({ page }) => {
    const facts: Record<string, unknown>[] = []
    const sent = await routeDeclarations(page, facts,
      db => peakSuggestion(db, 'mon 09:00-12:00 UTC'))
    await openFacts(page)
    await expect(page.getByTestId('brief-current')).toContainText('No owner brief yet')
    await expect(page.getByTestId('brief-suggestion')).toContainText('mon 09:00-12:00 UTC')
    await expect(page.getByTestId('brief-suggestion')).toContainText('3 rollup hours')
    await expect(page.getByTestId('brief-peak-hint')).toContainText('weekdays 09:00-17:00')
    await page.getByTestId('brief-owner').fill('Payments')
    await page.getByTestId('brief-contact').fill('#payments-oncall')
    await page.getByTestId('brief-suggestion-apply').click()
    await expect(page.getByTestId('brief-peak-windows')).toHaveValue('mon 09:00-12:00 UTC')
    await page.getByTestId('brief-save').click()
    await expect(page.getByTestId('brief-success')).toContainText('owner brief: owner Payments')
    expect(sent).toEqual([{ url: '/api/v1/facts?database=primary', body: {
      type: 'owner_brief', subject_kind: 'database', subject: '*', note: '',
      value: { owner: 'Payments', contact: '#payments-oncall',
        peak_windows: 'mon 09:00-12:00 UTC' } } }])
    // The saved brief is read again: the suggestion now matches it.
    await expect(page.getByTestId('brief-suggestion-match')).toBeVisible()
    await expect(page.getByTestId('brief-current')).toContainText('owner Payments')
  })

test('an existing brief is edited in place and its other fields are kept',
  async ({ page }) => {
    const facts = [ownerBrief(5, 'primary', { owner: 'Payments', growth_pct_month: '5' })]
    const sent = await routeDeclarations(page, facts,
      db => peakSuggestion(db, null, 'less than 28 days of hourly rollup'))
    await openFacts(page)
    await expect(page.getByTestId('brief-owner')).toHaveValue('Payments')
    await expect(page.getByTestId('brief-suggestion-none'))
      .toContainText('less than 28 days of hourly rollup')
    await expect(page.getByTestId('brief-suggestion-apply')).toHaveCount(0)
    await page.getByTestId('brief-retention').fill('7 years')
    await page.getByTestId('brief-save').click()
    await expect(page.getByTestId('brief-success')).toBeVisible()
    expect(sent.map(d => d.body.value)).toEqual([
      { owner: 'Payments', growth_pct_month: '5', retention: '7 years' }])
    expect(facts.filter(f => f.type === 'owner_brief')).toHaveLength(1)
  })

test('declares a do-not-touch fact on a table', async ({ page }) => {
  const facts: Record<string, unknown>[] = []
  const sent = await routeDeclarations(page, facts, db => peakSuggestion(db, null, 'none'))
  await openFacts(page)
  await page.getByTestId('declare-type').selectOption({ label: 'Do not touch' })
  const kinds = await page.getByTestId('declare-kind').locator('option').allTextContents()
  expect(kinds).toEqual(['table', 'index', 'schema'])
  await page.getByTestId('declare-subject').fill('public.orders')
  await page.getByTestId('declare-submit').click()
  await expect(page.getByTestId('declare-success'))
    .toContainText('table public.orders must not be touched')
  expect(sent).toEqual([{ url: '/api/v1/facts?database=primary', body: {
    type: 'do_not_touch', subject_kind: 'table', subject: 'public.orders', value: {},
    note: '' } }])
  await page.getByTestId('facts-tab-confirmed').click()
  await expect(page.getByTestId('fact-card-100')).toContainText('must not be touched')
})
