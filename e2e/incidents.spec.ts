import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const PASSWORD = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test('incident cases preserve observed evidence and policy blocking reason', async ({ page }) => {
  const errors = getConsoleErrors(page);
  await login(page, EMAIL, PASSWORD);
  await page.route('**/api/v1/cases**', route => route.fulfill({ json: { cases: [{
    case_id: 'incident-review', source_type: 'incident', title: 'Autovacuum lag',
    severity: 'warning', state: 'open', why_now: 'Dead tuples exceed the observed threshold',
    evidence: [{ type: 'incident', summary: 'Vacuum has fallen behind',
      detail: { confidence: 0.87, affected_object: 'public.orders' } }],
    action_candidates: [{ action_type: 'vacuum', blocked_reason: 'Load telemetry unavailable',
      policy_decision: { decision: 'deny' } }],
  }] } }));
  await page.goto('/#/incidents');
  await expect(page.getByRole('button', { name: 'Incidents', exact: true }))
    .toHaveAttribute('aria-pressed', 'true');
  const card = page.locator('main article');
  await expect(card).toContainText('Dead tuples exceed the observed threshold');
  await expect(card.getByLabel('Case evidence')).toContainText('confidence: 0.87');
  await expect(card.getByLabel('Case evidence')).toContainText('public.orders');
  await expect(card).toContainText('vacuum: Load telemetry unavailable');
  await expect(card).toContainText('Policy: deny');
  expect(errors).toEqual([]);
});
