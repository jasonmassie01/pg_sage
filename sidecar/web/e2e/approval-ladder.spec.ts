// approval-ladder.spec.ts — lab-03a: a pending drop card shows what the
// change was tested on before production, and the ledger row shows the
// highest tier passed.
//
// The card's ladder is the golden that the Go loader test
// (internal/approvalcard/ladder_db_test.go) pins against a real database,
// so the mock serves exactly what the server serves.
// No concurrency tests: the card and the ledger are read-only renders.

import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { mockAllAPIs, mockPendingActions } from './fixtures'
import { ledger, ledgerRow } from './mocks/outcomes'

const golden = JSON.parse(readFileSync(new URL(
  '../../internal/approvalcard/testdata/ladder_card.golden.json', import.meta.url),
'utf8'))

const pendingAction = mockPendingActions.pending[0]

const dropCard = {
  ...golden,
  queue_id: pendingAction.id,
  database: pendingAction.database_name,
  title: 'Drop unused index public.idx_old',
  targets: [],
  evidence: [{ kind: 'finding', label: 'Finding #42', value: 'info', ref: 'finding:42' }],
  rationale: { source: 'rule', text: 'The index has not been scanned in 30 days' },
  predicted_effect: {},
  sql: 'DROP INDEX CONCURRENTLY "public"."idx_old"',
  rollback: { class: 'forward_fix_only', sql: 'CREATE INDEX CONCURRENTLY idx_old ON t (a)' },
  risk: { tier: 'moderate', blast_radius: 'one index', lock: 'SHARE UPDATE EXCLUSIVE',
    guardrails: ['approval_required'], post_checks: [] },
  proposed_at: '2026-11-16T09:00:00Z',
  expires_at: '2099-01-01T00:00:00Z',
  card_hash: 'hash-ladder',
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('pending drop card shows each tier it was tested on', async ({ page }) => {
  await page.route('**/api/v1/approvals**', route =>
    route.fulfill({ json: { cards: [dropCard] } }),
  )
  await page.goto('/#/actions')
  await page.getByTestId('actions-tab-pending').click()
  const items = page.getByTestId('approval-ladder').getByRole('listitem')
  await expect(items).toHaveCount(2)
  await expect(items.nth(0)).toHaveText('Tested on plans only: planned cost -41%')
  await expect(items.nth(1)).toHaveText('Skipped: no clone provider configured')
  await expect(page.getByTestId('approval-why')).toContainText('not rehearsed on a clone')
})

test('a card from before the ladder says no test was recorded', async ({ page }) => {
  const legacy = { ...dropCard, ladder: undefined,
    ladder_note: 'No pre-production test recorded' }
  await page.route('**/api/v1/approvals**', route =>
    route.fulfill({ json: { cards: [legacy] } }),
  )
  await page.goto('/#/actions')
  await page.getByTestId('actions-tab-pending').click()
  await expect(page.getByTestId('approval-ladder'))
    .toContainText('No pre-production test recorded')
})

test('the ledger row shows the highest tier passed', async ({ page }) => {
  const rows = [{ ...ledgerRow, tier: 'clone' }]
  await page.route(url => url.pathname === '/api/v1/outcomes', route =>
    route.fulfill({ json: { ...ledger, rows } }))
  await page.goto('/#/ledger')
  await expect(page.getByTestId('ledger-tier-primary-12')).toHaveText('Tested on a clone')
})
