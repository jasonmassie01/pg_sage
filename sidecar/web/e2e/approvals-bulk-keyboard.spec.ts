// approvals-bulk-keyboard.spec.ts (ux-07b): on Waiting on you, the
// keyboard alone selects safe-tier cards (x), moves (j, k), opens the bulk
// confirm (a with a selection) and confirms it. The confirm lists each SQL
// statement with its database, and the request carries each card's own
// hash. The cards copy the shape approvalcard.Card serves, as
// actions-quickwins.spec.ts does.
// No concurrency tests here: Playwright key presses are serial; the
// overlap of a bulk and a single approval is a Go DB test.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs, mockPendingActions } from './fixtures'

const base = mockPendingActions.pending[0]
const db = base.database_name
const ids = [61, 62, 63]
const queue = ids.map(id => ({ ...base, id, policy_decision: 'queue_for_approval',
  lifecycle_state: 'pending', blocked_reason: '', eligible: true }))

const cardFor = (id: number) => ({
  queue_id: id, database: db, title: `Analyze table t${id}`,
  action_type: 'analyze_table', status: 'pending', targets: [],
  evidence: [{ kind: 'finding', label: 'Finding #42', value: 'warning', ref: 'finding:42' }],
  rationale: { source: 'rule', text: 'Statistics are stale' }, predicted_effect: {},
  sql: `ANALYZE public.t${id}`,
  rollback: { class: 'no_rollback_needed', sql: '', note: 'Nothing to undo' },
  risk: { tier: 'safe', blast_radius: 'one table', guardrails: [], post_checks: [] },
  why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
  proposed_at: '2026-11-16T09:00:00Z', expires_at: '2099-01-01T00:00:00Z',
  card_hash: `hash-${id}`,
})

async function routeCards(page: Page) {
  const bulk: unknown[] = []
  const single: string[] = []
  await page.route('**/api/v1/actions**', route => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/v1/actions/pending/count') {
      return route.fulfill({ json: { count: queue.length } })
    }
    if (url.pathname === '/api/v1/actions/pending') {
      return route.fulfill({ json: { pending: queue, scheduled: [], total: queue.length } })
    }
    return route.fulfill({ json: { actions: [], total: 0 } })
  })
  await page.route('**/api/v1/approvals**', route => {
    const req = route.request()
    const url = new URL(req.url())
    if (req.method() === 'POST' && url.pathname === '/api/v1/approvals/bulk-approve') {
      bulk.push(req.postDataJSON())
      return route.fulfill({ json: { approved: 1, scheduled: 0, refused: 1, failed: 0,
        results: [
          { id: 61, database: db, status: 'approved', executed: true },
          { id: 62, database: db, status: 'refused', executed: false, code: 'not_eligible',
            reason: 'the finding behind this action is resolved' }] } })
    }
    if (req.method() === 'POST') {
      single.push(url.pathname)
      return route.fulfill({ json: { ok: true, executed: true } })
    }
    return route.fulfill({ json: { cards: ids.map(cardFor) } })
  })
  return { bulk, single }
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('selects two safe cards, confirms the bulk and keeps the refused one, by keyboard',
  async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 })
    const posts = await routeCards(page)
    await page.goto('/#/actions')
    await expect(page.getByTestId('approval-card')).toHaveCount(3)
    await page.keyboard.press('x')
    await page.keyboard.press('j')
    await page.keyboard.press('j')
    await page.keyboard.press('k')
    await page.keyboard.press('x')
    await expect(page.getByTestId(`bulk-select-${db}-61`)).toBeChecked()
    await expect(page.getByTestId(`bulk-select-${db}-62`)).toBeChecked()
    await expect(page.getByTestId(`bulk-select-${db}-63`)).not.toBeChecked()
    await page.keyboard.press('a')
    const dialog = page.getByRole('dialog', { name: 'Approve 2 safe changes' })
    await expect(dialog).toBeVisible()
    const rows = dialog.getByTestId('bulk-approve-item')
    await expect(rows).toHaveCount(2)
    await expect(rows.nth(0)).toContainText('ANALYZE public.t61')
    await expect(rows.nth(1)).toContainText('ANALYZE public.t62')
    await expect(rows.nth(0)).toContainText(db)
    // Keys do nothing behind the dialog.
    await page.keyboard.press('j')
    await page.keyboard.press('x')
    expect(posts.bulk).toEqual([])
    await expect(page.getByTestId('bulk-approve-dialog-cancel')).toBeFocused()
    await page.keyboard.press('Tab')
    await expect(page.getByTestId('bulk-approve-dialog-confirm')).toBeFocused()
    await page.keyboard.press('Enter')
    await expect(dialog).toBeHidden()
    expect(posts.bulk).toEqual([{ items: [
      { id: 61, database: db, card_hash: 'hash-61' },
      { id: 62, database: db, card_hash: 'hash-62' }] }])
    expect(posts.single).toEqual([])
    await expect(page.getByTestId(`bulk-result-${db}-62`))
      .toContainText('the finding behind this action is resolved')
    await expect(page.getByTestId(`bulk-select-${db}-62`)).toBeChecked()
    await expect(page.getByTestId('bulk-approve-button')).toHaveText('Approve selected (1)')
  })

test('a without a selection still approves only the selected card', async ({ page }) => {
  const posts = await routeCards(page)
  await page.goto('/#/actions')
  await expect(page.getByTestId('approval-card')).toHaveCount(3)
  await page.keyboard.press('j')
  await page.keyboard.press('a')
  const dialog = page.getByRole('dialog', { name: 'Approve: Analyze table t62' })
  await expect(dialog).toBeVisible()
  await dialog.getByTestId('approval-key-confirm-confirm').click()
  await expect(dialog).toBeHidden()
  expect(posts.single).toEqual(['/api/v1/approvals/62/approve'])
  expect(posts.bulk).toEqual([])
})
