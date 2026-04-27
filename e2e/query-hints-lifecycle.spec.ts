import { execFileSync } from 'child_process';
import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

function psql(sql: string) {
  execFileSync('docker', [
    'exec', 'pg_sage-pg-target-1', 'psql',
    '-v', 'ON_ERROR_STOP=1',
    '-U', 'postgres',
    '-d', 'testdb',
    '-c', sql,
  ], { stdio: 'pipe' });
}

function seedHintLifecycleRows() {
  psql(`
    DELETE FROM sage.query_hints
     WHERE hint_text LIKE 'codex lifecycle %';

    INSERT INTO sage.query_hints
      (queryid, hint_text, symptom, status, before_cost, after_cost,
       created_at, verified_at, rolled_back_at)
    VALUES
      (910001, 'codex lifecycle active', 'seq_scan', 'active',
       100, 70, now(), NULL, NULL),
      (910002, 'codex lifecycle retired', 'seq_scan', 'retired',
       100, 60, now() - interval '1 minute', now(), NULL),
      (910003, 'codex lifecycle broken', 'seq_scan', 'broken',
       100, NULL, now() - interval '2 minutes', NULL, now());
  `);
}

test.describe('Query Hints lifecycle', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('API and UI show active, retired, and broken hints', async ({
    page,
  }) => {
    try {
      seedHintLifecycleRows();
    } catch (err) {
      test.skip(true, `Docker fixture database unavailable: ${err}`);
    }

    const allRes = await page.request.get('/api/v1/query-hints');
    expect(allRes.status()).toBe(200);
    const all = await allRes.json();
    const lifecycle = (all.hints || []).filter((h: any) =>
      String(h.hint_text).startsWith('codex lifecycle '),
    );
    expect(new Set(lifecycle.map((h: any) => h.status))).toEqual(
      new Set(['active', 'retired', 'broken']),
    );

    const activeRes = await page.request.get(
      '/api/v1/query-hints?status=active',
    );
    expect(activeRes.status()).toBe(200);
    const active = await activeRes.json();
    const activeLifecycle = (active.hints || []).filter((h: any) =>
      String(h.hint_text).startsWith('codex lifecycle '),
    );
    expect(activeLifecycle).toHaveLength(1);
    expect(activeLifecycle[0].status).toBe('active');

    await page.goto('/#/query-hints');
    await expect(page.getByTestId('query-hints-table')).toBeVisible();
    await expect(page.getByText('active').first()).toBeVisible();
    await expect(page.getByText('retired').first()).toBeVisible();
    await expect(page.getByText('broken').first()).toBeVisible();
  });
});
