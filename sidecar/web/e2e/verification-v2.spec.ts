// verification-v2.spec.ts (lab-01c CHECK-10): an executed action's
// verification outcome in the real bundle: the v2 labels ("No change",
// "Inconclusive"), the effect with its CI against the controls, and in
// shadow the "v2 would say" line under the acted-on verdict. The outcome
// copies what actionOutcomeColumnSQL returns (verifier, other_verdict,
// other_evidence beside the v1 fields). Payloads: e2e/mocks/actions.ts.
// No concurrency tests: each test drives one page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { executedRow, routeActions } from './mocks/actions'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

const v2Record = {
  verifier: 'v2', method: 'did', basis: 'seasonal', effect_pct: -38, ci_low_pct: -45.1,
  ci_high_pct: -30.2, controls: 12, outcome: 'improved',
}

function outcomeRow(outcome: Record<string, unknown>) {
  return executedRow({ outcome: 'success', rollback_reason: null,
    verification_outcome: {
      class: 'index_create', tolerance: 'met', reason: 'index used by its targets',
      predicted: { method: 'hypopg', metric: 'mean_exec_time', expected_change_pct: -40 },
      observed: { metric: 'mean_exec_time', before: 20, after: 12, change_pct: -40 },
      evidence: { v2: v2Record }, ...outcome,
    } })
}

async function openOutcome(page: Page) {
  await page.goto('/#/actions')
  await page.getByTestId('actions-tab-executed').click()
  const table = page.getByTestId('executed-actions-table')
  await table.getByRole('button', { name: 'Expand row' }).first().click()
  return page.getByTestId('verification-outcome')
}

test('lab-01c CHECK-10: v2 acted: the effect with its CI vs the controls', async ({ page }) => {
  await routeActions(page, { ledger: () => [outcomeRow({ verdict: 'improved',
    verifier: 'v2', other_verdict: 'improved', other_evidence: {} })] })
  const panel = await openOutcome(page)
  await expect(panel.getByTestId('outcome-verdict')).toHaveText('Improved')
  await expect(panel.getByTestId('outcome-method')).toContainText('Verifier v2')
  await expect(panel.getByTestId('outcome-effect')).toHaveText(
    '-38.0% (CI -45.1% to -30.2%) vs 12 control statements')
  await expect(panel.getByTestId('outcome-shadow')).toHaveCount(0)
})

test('shadow: v1 acted and the v2 line says what v2 would say', async ({ page }) => {
  const neutral = { ...v2Record, outcome: 'neutral', effect_pct: -2, ci_low_pct: -6,
    ci_high_pct: 3 }
  await routeActions(page, { ledger: () => [outcomeRow({ verdict: 'improved',
    verifier: 'v1', other_verdict: 'neutral', other_evidence: { v2: neutral },
    evidence: { v2: neutral } })] })
  const panel = await openOutcome(page)
  await expect(panel.getByTestId('outcome-verdict')).toHaveText('Improved')
  await expect(panel.getByTestId('outcome-shadow')).toHaveText('v2 would say: No change')
})

test('the v2 label No change', async ({ page }) => {
  await routeActions(page, { ledger: () => [outcomeRow({ verdict: 'neutral',
    verifier: 'v2' })] })
  const panel = await openOutcome(page)
  await expect(panel.getByTestId('outcome-verdict')).toHaveText('No change')
})

test('the v2 label Inconclusive', async ({ page }) => {
  await routeActions(page, { ledger: () => [outcomeRow({
    verdict: 'insufficient_evidence', verifier: 'v2' })] })
  const panel = await openOutcome(page)
  await expect(panel.getByTestId('outcome-verdict')).toHaveText('Inconclusive')
})
