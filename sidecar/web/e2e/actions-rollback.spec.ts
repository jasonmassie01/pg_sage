// actions-rollback.spec.ts (dashboard-14): the "Retry rollback" row of the
// Actions ledger (executor-07 CHECK-31) in the real bundle with real
// clicks. A withheld (rollback_skipped) or failed rollback offers Retry,
// shows why it did not run, and posts only after Confirm with today's body.
// Payloads: e2e/mocks/actions.ts.
// No concurrency tests: each test drives one page and one dialog.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { executedRow, rollbackBody, rollbackReason, routeActions } from './mocks/actions'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

async function openExecutedRow(page: Page) {
  await page.goto('/#/actions')
  await page.getByTestId('actions-tab-executed').click()
  const table = page.getByTestId('executed-actions-table')
  await expect(table).toBeVisible()
  await table.getByRole('button', { name: 'Expand row' }).first().click()
  return table
}

test('executor-07 CHECK-31: a rollback_skipped row offers Retry rollback with its reason',
  async ({ page }) => {
    const posts = await routeActions(page, { ledger: () => [executedRow()] })
    const table = await openExecutedRow(page)
    await expect(table.getByText('Rollback Reason')).toBeVisible()
    await expect(table.getByText('emergency stop active; automatic rollback withheld'))
      .toBeVisible()
    const button = page.getByTestId('rollback-action-button')
    await expect(button).toHaveText('Retry rollback')

    await button.click()
    const dialog = page.getByRole('dialog', { name: 'Roll back action 9' })
    await expect(dialog).toBeVisible()
    await expect(dialog.getByTestId('confirm-dialog-sql'))
      .toContainText('DROP INDEX CONCURRENTLY IF EXISTS idx_s')
    await expect(dialog.getByTestId('confirm-dialog-database')).toHaveText('Database: orders')
    expect(posts, 'nothing is sent before Confirm').toEqual([])
    // ux-01b: Confirm waits for the operator's reason.
    await expect(dialog.getByTestId('confirm-dialog-confirm')).toBeDisabled()
    await dialog.getByTestId('confirm-dialog-reason').fill(rollbackReason)

    await dialog.getByTestId('confirm-dialog-confirm').click()
    await expect(dialog).toBeHidden()
    expect(posts).toEqual([
      { url: '/api/v1/actions/9/rollback?database=orders', body: rollbackBody },
    ])
  })

test('Retry moves the row from rollback_skipped to Rolled Back', async ({ page }) => {
  let outcome = 'rollback_skipped'
  const posts = await routeActions(page, {
    ledger: () => [executedRow({ outcome,
      rollback_reason: outcome === 'rolled_back' ? rollbackReason
        : 'emergency stop active; automatic rollback withheld' })],
    rollback: () => {
      outcome = 'rolled_back'
      return { status: 200, json: { ok: true, action_id: 9, status: 'rolled_back' } }
    },
  })
  const table = await openExecutedRow(page)
  await expect(table.getByRole('row').nth(1)).toContainText('rollback_skipped')
  await page.getByTestId('rollback-action-button').click()
  await page.getByTestId('confirm-dialog-reason').fill(rollbackReason)
  await page.getByTestId('confirm-dialog-confirm').click()

  await expect(page.getByText('Action 9 rolled back')).toBeVisible()
  await expect(table.getByRole('row').nth(1)).toContainText('Rolled Back')
  await expect(table.getByRole('row').nth(1)).not.toContainText('rollback_skipped')
  // A rolled-back row can no longer be rolled back: the retry is gone.
  const expander = table.getByTestId('row-expander').first()
  if (await expander.getAttribute('aria-expanded') !== 'true') await expander.click()
  await expect(expander).toHaveAttribute('aria-expanded', 'true')
  await expect(table.getByText(rollbackReason)).toBeVisible()
  await expect(page.getByTestId('rollback-action-button')).toHaveCount(0)
  expect(posts).toHaveLength(1)
})

test('a rollback_failed row offers Retry rollback too', async ({ page }) => {
  await routeActions(page, {
    ledger: () => [executedRow({ outcome: 'rollback_failed',
      rollback_reason: 'DROP INDEX failed: lock timeout' })],
  })
  const table = await openExecutedRow(page)
  await expect(table.getByText('DROP INDEX failed: lock timeout')).toBeVisible()
  await expect(page.getByTestId('rollback-action-button')).toHaveText('Retry rollback')
})

test('an abandoned revert offers no retry', async ({ page }) => {
  await routeActions(page, {
    ledger: () => [executedRow({
      rollback_reason: 'revert refused: the index is no longer owned by pg_sage' })],
  })
  const table = await openExecutedRow(page)
  await expect(table.getByText(/revert refused: the index is no longer owned/)).toBeVisible()
  await expect(page.getByTestId('rollback-action-button')).toHaveCount(0)
})

test('an executed row with no stored rollback SQL shows a disabled button and why',
  async ({ page }) => {
    const posts = await routeActions(page, {
      ledger: () => [executedRow({ outcome: 'success', rollback_sql: '',
        rollback_reason: '' })],
    })
    await openExecutedRow(page)
    const button = page.getByTestId('rollback-action-button')
    await expect(button).toBeDisabled()
    await expect(button).toHaveText('Roll Back Action')
    await expect(page.getByTestId('rollback-unavailable-reason'))
      .toHaveText('No rollback SQL was stored for this action, so pg_sage cannot roll it back.')
    expect(posts).toEqual([])
  })
