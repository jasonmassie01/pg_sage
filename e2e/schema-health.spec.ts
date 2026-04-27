import { test, expect } from '@playwright/test';
import { execFileSync } from 'child_process';
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

function seedSchemaFinding() {
  psql(`
    DELETE FROM sage.findings
     WHERE object_identifier = 'codex_schema_health.detail';

    INSERT INTO sage.findings
      (category, severity, object_type, object_identifier, title,
       detail, recommendation, recommended_sql, status, rule_id,
       impact_score)
    VALUES
      ('schema_lint:codex_detail', 'critical', 'table',
       'codex_schema_health.detail',
       'Codex schema detail finding',
       '{"thematic_category":"safety","schema_name":"public","table_name":"codex_schema_health","database_name":"testdb","impact":"Codex impact text"}',
       'Codex recommendation text',
       'ALTER TABLE public.codex_schema_health ADD PRIMARY KEY (id);',
       'open', 'codex_detail', 0.91);
  `);
}

test.describe('Schema Health', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('applies filters to both stats and findings requests', async ({
    page,
  }) => {
    const statsUrls: string[] = [];
    const findingsUrls: string[] = [];

    await page.route('**/api/v1/findings/stats?**', async (route) => {
      const url = route.request().url();
      statsUrls.push(url);
      const parsed = new URL(url);
      const filtered = parsed.searchParams.get('severity') === 'critical'
        && parsed.searchParams.get('thematic_category') === 'indexing';
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          database: 'all',
          stats: filtered
            ? [{ severity: 'critical', category: 'schema_lint:x', count: 1 }]
            : [{ severity: 'warning', category: 'schema_lint:y', count: 2 }],
          total_open: filtered ? 1 : 2,
        }),
      });
    });

    await page.route('**/api/v1/findings?**', async (route) => {
      findingsUrls.push(route.request().url());
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          total: 0,
          findings: [],
        }),
      });
    });

    await page.goto('/#/schema-health');
    await expect(page.getByTestId('stats-summary')).toContainText('2');

    await page.getByTestId('severity-filter').selectOption('critical');
    await page.getByTestId('category-filter').selectOption('indexing');

    await expect(page.getByTestId('stats-summary')).toContainText('1');
    expect(statsUrls.at(-1)).toContain('severity=critical');
    expect(statsUrls.at(-1)).toContain('thematic_category=indexing');
    expect(findingsUrls.at(-1)).toContain('severity=critical');
    expect(findingsUrls.at(-1)).toContain('thematic_category=indexing');
  });

  test('expands a live schema finding with impact and SQL detail', async ({
    page,
  }) => {
    try {
      seedSchemaFinding();
    } catch (err) {
      test.skip(true, `Docker fixture database unavailable: ${err}`);
    }

    await page.goto('/#/schema-health');
    await page.getByTestId('category-filter').selectOption('safety');
    await expect(page.getByText('Codex schema detail finding'))
      .toBeVisible();
    await expect(page.getByTestId('schema-findings-table')).toBeVisible();
    await page.locator('[data-row-key]').filter({
      hasText: 'Codex schema detail finding',
    }).click();

    await expect(page.getByTestId('finding-detail')).toBeVisible();
    await expect(page.getByText('Codex impact text')).toBeVisible();
    await expect(page.getByText('Codex recommendation text')).toBeVisible();
    await expect(page.getByText(
      'ALTER TABLE public.codex_schema_health ADD PRIMARY KEY (id);',
    )).toBeVisible();
  });
});
