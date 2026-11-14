// approval-hint-plans.spec.ts — tuning-05b: a pending pg_hint_plan card
// shows the plan comparison that gated the hint and both plans.
//
// The card's hint_evidence is the golden that the Go loader test
// (internal/approvalcard/hint_evidence_db_test.go) pins against a real
// database, so the mock serves exactly what the server serves.

import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { mockAllAPIs, mockPendingActions } from './fixtures'

const golden = JSON.parse(readFileSync(new URL(
  '../../internal/approvalcard/testdata/hint_evidence_card.golden.json', import.meta.url),
'utf8'))

const pendingAction = mockPendingActions.pending[0]

const hintCard = {
  ...golden,
  queue_id: pendingAction.id,
  database: pendingAction.database_name,
  targets: [],
  evidence: [{ kind: 'finding', label: 'Finding #42', value: 'warning', ref: 'finding:42' }],
  rationale: { source: 'rule', text: 'Pin the index scan' },
  predicted_effect: {},
  sql: "INSERT INTO hint_plan.hints (query_id, application_name, hints) " +
    "VALUES (4242, '', 'IndexScan(orders idx_orders_customer)')",
  rollback: { class: 'reversible', sql: 'DELETE FROM hint_plan.hints WHERE query_id = 4242' },
  risk: { tier: 'moderate', blast_radius: 'one query', lock: 'no locks',
    guardrails: ['approval_required'], post_checks: [] },
  why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
  proposed_at: '2026-11-01T11:00:00Z',
  expires_at: '2099-01-01T00:00:00Z',
  card_hash: 'hash-hint',
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.route('**/api/v1/approvals**', route =>
    route.fulfill({ json: { cards: [hintCard] } }),
  )
})

test('pending hint card shows the plan comparison and both plans', async ({ page }) => {
  await page.goto('/#/actions')
  await page.getByTestId('actions-tab-pending').click()
  const section = page.getByTestId('approval-hint-plans')
  await expect(section.getByTestId('hint-plan-summary'))
    .toHaveText('Plan comparison: changed, cost 412.0 to 254.6 (-38.2%)')

  const plain = section.getByTestId('hint-plan-plain')
  const hinted = section.getByTestId('hint-plan-hinted')
  await expect(plain.locator('pre')).not.toBeVisible()
  await expect(hinted.locator('pre')).not.toBeVisible()

  await plain.getByText('Plan without hint').click()
  await expect(plain.locator('pre')).toBeVisible()
  await expect(plain.locator('pre')).toContainText('"Node Type": "Seq Scan"')

  await hinted.getByText('Plan with hint').click()
  await expect(hinted.locator('pre')).toBeVisible()
  await expect(hinted.locator('pre')).toContainText('"Index Name": "idx_orders_customer"')
})
