import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const PASSWORD = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Findings in Cases', () => {
  let errors: string[];
  test.beforeEach(async ({ page }) => {
    errors = getConsoleErrors(page);
    await login(page, EMAIL, PASSWORD);
    await page.route('**/api/v1/cases**', route => route.fulfill({ json: { cases: [{
      case_id: 'pending-finding', source_type: 'finding', title: 'Orders index review',
      severity: 'warning', state: 'open', impact_score: 0.8,
      action_candidates: [{ action_type: 'create_index', blocked_reason: 'Review required',
        policy_decision: { decision: 'require_approval' }, guardrails: ['No automatic DDL'] }],
      actions: [{ id: 91, type: 'create_index', status: 'pending_approval',
        lifecycle_state: 'awaiting_review', verification_status: 'not_started',
        blocked_reason: 'Approval required' }],
    }] } }));
    await page.goto('/#/findings');
  });
  test.afterEach(() => expect(errors).toEqual([]));

  test('legacy findings route exposes current Cases filters and count', async ({ page }) => {
    await expect(page.locator('main header h1')).toHaveText('Cases');
    await expect(page.getByTestId('cases-page-description')).toContainText('1 of 1');
    await page.getByRole('button', { name: 'Schema', exact: true }).click();
    await expect(page.locator('main article')).toHaveCount(0);
    await page.getByRole('button', { name: 'Findings', exact: true }).click();
    await expect(page.locator('main article')).toHaveCount(1);
  });

  test('pending finding shows guarded action timeline without direct execution', async ({ page }) => {
    const card = page.locator('main article');
    await expect(card).toContainText('create_index: Review required');
    await expect(card).toContainText('Policy: require_approval');
    await expect(card.getByLabel('Action guardrails')).toContainText('No automatic DDL');
    await expect(card.getByLabel('Action timeline')).toContainText('pending_approval');
    await expect(card.getByLabel('Action timeline')).toContainText('Approval required');
    await expect(card.getByRole('button', { name: 'Take Action' })).toHaveCount(0);
  });
});
