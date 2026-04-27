import { execFileSync } from 'child_process';
import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

const targets = [
  { name: 'testdb', container: 'pg_sage-pg-target-1', db: 'testdb' },
  { name: 'testdb2', container: 'pg_sage-pg-target-2-1', db: 'testdb2' },
  { name: 'health_test', container: 'health_pg', db: 'health_test' },
];

function psql(target: typeof targets[number], sql: string) {
  execFileSync('docker', [
    'exec', target.container, 'psql',
    '-v', 'ON_ERROR_STOP=1',
    '-U', 'postgres',
    '-d', target.db,
    '-c', sql,
  ], { stdio: 'pipe' });
}

function seedFleetRows() {
  for (const [i, target] of targets.entries()) {
    psql(target, `
      DELETE FROM sage.query_hints
       WHERE hint_text LIKE 'codex aggregation %';
      DELETE FROM sage.action_log
       WHERE sql_executed LIKE '/* codex aggregation %';
      DELETE FROM sage.incidents
       WHERE root_cause LIKE 'codex aggregation %';

      INSERT INTO sage.query_hints
        (queryid, hint_text, symptom, status, created_at)
      VALUES
        (${900001 + i}, 'codex aggregation ${target.name}',
         'seq_scan', 'active', now() - interval '${i} minutes');

      INSERT INTO sage.action_log
        (action_type, sql_executed, outcome, executed_at)
      VALUES
        ('verify', '/* codex aggregation ${target.name} */ SELECT 1',
         'success', now() - interval '${i} minutes');

      INSERT INTO sage.incidents
        (id, severity, root_cause, source, confidence,
         database_name, detected_at)
      VALUES
        ('00000000-0000-0000-0000-00000009000${i + 1}',
         'warning', 'codex aggregation ${target.name}',
         'deterministic', 0.91, '${target.name}',
         now() - interval '${i} minutes')
      ON CONFLICT (id) DO UPDATE SET
        resolved_at = NULL,
        detected_at = EXCLUDED.detected_at;
    `);
  }
}

test.describe('Fleet aggregation APIs', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('all-database views include secondary database rows', async ({
    page,
  }) => {
    try {
      seedFleetRows();
    } catch (err) {
      test.skip(true, `Docker fixture databases unavailable: ${err}`);
    }

    const hintsRes = await page.request.get('/api/v1/query-hints');
    expect(hintsRes.status()).toBe(200);
    const hints = await hintsRes.json();
    const hintDBs = new Set(
      (hints.hints || [])
        .filter((h: any) => String(h.hint_text).startsWith(
          'codex aggregation ',
        ))
        .map((h: any) => h.database_name),
    );
    for (const target of targets) {
      expect(hintDBs.has(target.name)).toBeTruthy();
    }

    const actionsRes = await page.request.get('/api/v1/actions?limit=20');
    expect(actionsRes.status()).toBe(200);
    const actions = await actionsRes.json();
    const actionDBs = new Set(
      (actions.actions || [])
        .filter((a: any) => String(a.sql_executed).includes(
          'codex aggregation ',
        ))
        .map((a: any) => a.database_name),
    );
    for (const target of targets) {
      expect(actionDBs.has(target.name)).toBeTruthy();
    }

    const incidentsRes = await page.request.get(
      '/api/v1/incidents?status=active',
    );
    expect(incidentsRes.status()).toBe(200);
    const incidents = await incidentsRes.json();
    const incidentDBs = new Set(
      (incidents.incidents || [])
        .filter((i: any) => String(i.root_cause).startsWith(
          'codex aggregation ',
        ))
        .map((i: any) => i.database_name),
    );
    for (const target of targets) {
      expect(incidentDBs.has(target.name)).toBeTruthy();
    }
  });
});
