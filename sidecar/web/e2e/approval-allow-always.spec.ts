// approval-allow-always.spec.ts — ux-07a: an approval card offers
// "Approve, and stop asking for <class> on <database>" only when the
// server says so (trust.allow_always); pressing it confirms, then posts
// the card hash and the card's class to approve-and-allow, and says
// whether the class now runs unattended or the approval stood alone.
//
// Mocked: the visibility rules themselves are decided by the server
// (earned.AllowAlways, pinned by internal/earned and the route's DB
// tests); this spec pins that the page follows them exactly.
// No concurrency tests: one decision per card is in flight at a time
// (ApprovalActions), and the race of two people is a DB test.

import { expect, test } from '@playwright/test'
import { mockAllAPIs, mockPendingActions } from './fixtures'

const pendingAction = mockPendingActions.pending[0]

const offered = {
  family: 'hygiene', class: 'analyze', level: 'L2', cap: 'L3', allow_always: true,
  allow_always_label: 'Approve, and stop asking for analyze on primary',
  allow_always_why: '14 person approvals with no rollback',
  approval_history: { approvals: 14, required: 10, qualifies: true },
  evidence: { improved: 14, neutral: 0, regressed: 0, rolled_back: 0, rejected: 0 },
  line: 'Trust: hygiene/analyze at L2 (cap L3); evidence 14 improved',
}

function card(trust: Record<string, unknown> | undefined) {
  return {
    queue_id: pendingAction.id, database: pendingAction.database_name,
    title: 'ANALYZE public.orders', status: 'pending', targets: ['public.orders'],
    evidence: [], why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
    rationale: { source: 'rule', text: 'Statistics are stale' }, predicted_effect: {},
    sql: 'ANALYZE public.orders', rollback: { class: 'no_rollback_needed', sql: '' },
    risk: { tier: 'safe', blast_radius: 'one table', guardrails: [], post_checks: [] },
    proposed_at: '2026-11-16T09:00:00Z', expires_at: '2099-01-01T00:00:00Z',
    card_hash: 'hash-allow', trust,
  }
}

type Posted = { url: string, body: Record<string, unknown> }

async function serveCards(page, shown, answer?: Record<string, unknown>) {
  const posted: Posted[] = []
  await page.route('**/api/v1/approvals**', route => {
    const req = route.request()
    if (req.method() === 'POST') {
      posted.push({ url: req.url(), body: req.postDataJSON() })
      return route.fulfill({ json: answer || {} })
    }
    return route.fulfill({ json: { cards: [shown] } })
  })
  await page.goto('/#/actions')
  await page.getByTestId('actions-tab-pending').click()
  await expect(page.getByTestId('approval-card')).toHaveCount(1)
  return posted
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('the card offers allow-always only when the server does', async ({ page }) => {
  await serveCards(page, card(offered))
  await expect(page.getByTestId('approval-allow-always'))
    .toHaveText('Approve, and stop asking for analyze on primary')
  await expect(page.getByTestId('approval-approve')).toBeVisible()
})

for (const [name, trust] of [
  ['nine approvals', { ...offered, allow_always: false, allow_always_label: undefined,
    allow_always_why: '9 of 10 person approvals with no rollback' }],
  ['an operator viewing it', { ...offered, allow_always: false,
    allow_always_label: undefined,
    allow_always_why: 'only an admin signed in as a person can stop the asking' }],
  ['no ledger', undefined],
] as const) {
  test(`no allow-always button with ${name}`, async ({ page }) => {
    await serveCards(page, card(trust as Record<string, unknown> | undefined))
    await expect(page.getByTestId('approval-approve')).toBeVisible()
    await expect(page.getByTestId('approval-allow-always')).toHaveCount(0)
  })
}

test('pressing it confirms, then grants and says so', async ({ page }) => {
  const posted = await serveCards(page, card(offered), { ok: true, executed: true,
    verification_status: 'verified', allow_always: true, autonomy_level: 'L3',
    database: 'primary', queue_id: pendingAction.id })
  await page.getByTestId('approval-allow-always').click()
  const dialog = page.getByTestId('approval-allow-always-confirm')
  await expect(dialog).toContainText('You approved analyze 14 times with no rollback')
  await expect(dialog).toContainText('unattended (L3)')
  expect(posted).toHaveLength(0)
  await page.getByTestId('approval-allow-always-confirm-confirm').click()
  await expect.poll(() => posted.length).toBe(1)
  expect(posted[0].url).toContain(
    `/api/v1/approvals/${pendingAction.id}/approve-and-allow?database=primary`)
  expect(posted[0].body).toEqual({ card_hash: 'hash-allow', family: 'hygiene',
    class: 'analyze' })
  await expect(page.getByText(/now runs unattended \(L3\)/)).toBeVisible()
})

test('cancelling the confirm sends nothing', async ({ page }) => {
  const posted = await serveCards(page, card(offered))
  await page.getByTestId('approval-allow-always').click()
  await page.getByTestId('approval-allow-always-confirm-cancel').click()
  await expect(page.getByTestId('approval-allow-always-confirm')).toBeHidden()
  expect(posted).toHaveLength(0)
})

test('a refused promotion says the approval stood', async ({ page }) => {
  await serveCards(page, card(offered), { ok: true, executed: true,
    verification_status: 'verified', allow_always: false,
    autonomy_error: 'approved; autonomy unchanged: autonomy proposal is not pending' })
  await page.getByTestId('approval-allow-always').click()
  await page.getByTestId('approval-allow-always-confirm-confirm').click()
  await expect(page.getByText(/approved; autonomy unchanged: autonomy proposal is not/))
    .toBeVisible()
})
