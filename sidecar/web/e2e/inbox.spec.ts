// inbox.spec.ts (ux-01b): the Inbox as the home page in the real bundle
// with real clicks: approve in place, Undo with a reason, the one badge.
// Payloads: e2e/mocks/inbox.ts.
// No concurrency tests here: two tabs marking the seen time at once is
// api.TestInboxSeenDB_ConcurrentMarksKeepTheLaterTime on PostgreSQL.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { inboxItem, ledgerRow, routeInbox } from './mocks/inbox'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('home is the Inbox: approve in place, the item leaves and the badge drops',
  async ({ page }) => {
    const state = { items: [
      inboxItem('change_approval', '42', { title: 'Add index on orders(customer_id)' }),
      inboxItem('fact', '7', { title: 'idx_legacy belongs to billing', risk: 'safe' }),
      inboxItem('autonomy_promotion', 'p-3', { title: 'Let pg_sage add indexes' }),
    ], rows: [] }
    const posts = await routeInbox(page, state)
    await page.goto('/#/')
    await expect(page.locator('header h1')).toHaveText('Inbox')
    const badge = page.getByTestId('nav-inbox').getByTestId('nav-badge')
    await expect(badge).toHaveText('3')
    await expect(page.getByTestId('nav-actions').getByTestId('nav-badge')).toHaveCount(0)
    await expect(page.getByTestId(/^inbox-item-/)).toHaveCount(3)

    const item = page.getByTestId('inbox-item-change_approval-42')
    await item.getByRole('button', { name: 'Review' }).click()
    await item.getByTestId('approval-approve').click()
    await expect(page.getByTestId('inbox-item-change_approval-42')).toHaveCount(0)
    await expect(badge).toHaveText('2')
    const approve = posts.find(p => p.url.includes('/approve'))
    expect(approve?.url).toBe('/api/v1/approvals/42/approve?database=primary')
    expect(approve?.body).toEqual({ card_hash: 'h42' })
    expect(approve?.headers['x-sage-surface']).toBe('inbox')
  })

test('Undo asks why and sends the reason; the row shows it rolled back', async ({ page }) => {
  const posts = await routeInbox(page, { items: [], rows: [ledgerRow] })
  await page.goto('/#/')
  const row = page.getByTestId('inbox-change-primary-12')
  await row.getByRole('button', { name: 'Undo' }).click()
  const dialog = page.getByRole('dialog', { name: /Roll back action 12/ })
  await expect(dialog.getByTestId('confirm-dialog-sql'))
    .toContainText('DROP INDEX CONCURRENTLY IF EXISTS idx_c')
  const confirm = dialog.getByTestId('confirm-dialog-confirm')
  await dialog.getByTestId('confirm-dialog-reason').fill('  ')
  await expect(confirm).toBeDisabled()
  await dialog.getByTestId('confirm-dialog-reason').fill('checkout writes slowed down')
  await confirm.click()
  await expect(dialog).toBeHidden()
  await expect(row).toContainText('Rolled back: checkout writes slowed down')
  const rollback = posts.find(p => p.url.includes('/rollback'))
  expect(rollback?.url).toBe('/api/v1/actions/12/rollback?database=primary')
  expect(rollback?.body).toEqual({ reason: 'checkout writes slowed down',
    record_kind: 'executed' })
  expect(rollback?.headers['x-sage-surface']).toBe('inbox')
})

test('the badge caps at 99+, the goal state words the counts, #/value still works',
  async ({ page }) => {
    await routeInbox(page, { items: [], rows: [ledgerRow], total: 120 })
    await page.goto('/#/')
    await expect(page.getByTestId('nav-inbox').getByTestId('nav-badge')).toHaveText('99+')
    await expect(page.getByTestId('inbox-goal')).toContainText('Nothing needs you. 1 change')
    await expect(page.getByTestId('inbox-goal')).toContainText('all verified.')
    // ux-11: #/value opens the ledger's Value summary.
    await page.goto('/#/value')
    await expect(page.locator('header h1')).toHaveText('Outcome ledger')
    await expect(page.getByTestId('ledger-value')).toHaveAttribute('open', '')
  })

test('the page marks itself seen after 5 s visible', async ({ page }) => {
  const posts = await routeInbox(page, { items: [], rows: [] })
  const seen = page.waitForRequest(req => req.method() === 'POST'
    && new URL(req.url()).pathname === '/api/v1/inbox/seen', { timeout: 15000 })
  await page.goto('/#/')
  await seen
  await expect.poll(() => posts.filter(p => p.url === '/api/v1/inbox/seen').length).toBe(1)
  const mark = posts.find(p => p.url === '/api/v1/inbox/seen')
  expect(typeof (mark?.body as { seen_at?: string })?.seen_at).toBe('string')
})
