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

function seedHealthHistory() {
  for (const [i, target] of targets.entries()) {
    psql(target, `
      DELETE FROM sage.health_history
       WHERE recorded_at > now() - interval '2 hours';
      INSERT INTO sage.health_history
        (database_name, health_score, findings_open,
         findings_critical, findings_warning, findings_info,
         actions_total, recorded_at)
      VALUES
        ('${target.name}', ${95 - i * 10}, ${i + 1}, ${i},
         ${i + 2}, ${i + 3}, ${i + 4},
         now() - interval '10 minutes'),
        ('${target.name}', ${90 - i * 10}, ${i + 2}, ${i},
         ${i + 3}, ${i + 4}, ${i + 5},
         now() - interval '5 minutes');
    `);
  }
}

test.describe('Fleet Health chart', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('renders live health history for all local databases', async ({
    page,
  }) => {
    try {
      seedHealthHistory();
    } catch (err) {
      test.skip(true, `Docker fixture databases unavailable: ${err}`);
    }

    const res = await page.request.get('/api/v1/fleet/health?hours=2');
    expect(res.status()).toBe(200);
    const body = await res.json();
    for (const target of targets) {
      expect(body.databases[target.name]?.length).toBeGreaterThan(0);
    }

    await page.evaluate(() => {
      window.localStorage.setItem('pg_sage_range', '24h');
    });
    await page.goto('/#/');
    const chart = page.getByTestId('fleet-health-chart');
    await expect(chart).toBeVisible();
    for (const target of targets) {
      await expect(chart.getByText(target.name).first()).toBeVisible();
    }
  });
});
