import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const PASSWORD = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test('schema cases display evidence, migration, rollback, and verification SQL', async ({ page }) => {
  const errors = getConsoleErrors(page);
  await login(page, EMAIL, PASSWORD);
  await page.route('**/api/v1/cases**', route => route.fulfill({ json: { cases: [{
    case_id: 'schema-review', source_type: 'schema_health', title: 'Missing primary key',
    severity: 'critical', state: 'open', why: 'Writes need stable row identity',
    evidence: [{ type: 'schema', summary: 'orders has no primary key',
      detail: { query: 'SELECT id FROM public.orders;', table_name: 'orders' } }],
    action_candidates: [{ action_type: 'add_primary_key',
      script_output: { filename: 'orders_key.sql',
        migration_sql: 'ALTER TABLE public.orders ADD PRIMARY KEY (id);',
        rollback_sql: 'ALTER TABLE public.orders DROP CONSTRAINT orders_pkey;',
        verification_sql: ['SELECT count(*) FROM public.orders;'] } }],
  }] } }));
  await page.goto('/#/schema-health');
  await expect(page.getByRole('button', { name: 'Schema', exact: true }))
    .toHaveAttribute('aria-pressed', 'true');
  const card = page.locator('main article');
  await expect(card).toContainText('Writes need stable row identity');
  await expect(card.getByLabel('Case evidence')).toContainText('orders has no primary key');
  for (const sql of [
    'SELECT id FROM public.orders;',
    'ALTER TABLE public.orders ADD PRIMARY KEY (id);',
    'ALTER TABLE public.orders DROP CONSTRAINT orders_pkey;',
    'SELECT count(*) FROM public.orders;',
  ]) await expect(card).toContainText(sql);
  expect(errors).toEqual([]);
});
