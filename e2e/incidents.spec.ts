import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Incidents', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('renders confidence and resolves with valid JSON body', async ({
    page,
  }) => {
    let resolveBody: unknown = null;
    let active = true;

    await page.route('**/api/v1/incidents?**', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          incidents: active
            ? [{
                id: 'incident-1',
                severity: 'warning',
                root_cause: 'Autovacuum lag caused table bloat',
                source: 'schema_lint',
                database_name: 'primary',
                occurrence_count: 2,
                detected_at: new Date().toISOString(),
                confidence: 0.87,
                action_risk: 'high',
                causal_chain: [],
                signal_ids: ['bloat-high'],
                affected_objects: ['public.orders'],
              }]
            : [],
          total: active ? 1 : 0,
        }),
      });
    });

    await page.route('**/api/v1/incidents/incident-1/resolve',
      async (route) => {
        resolveBody = route.request().postDataJSON();
        active = false;
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            ok: true,
            id: 'incident-1',
            status: 'resolved',
          }),
        });
      });

    await page.goto('/#/incidents');
    await expect(page.getByTestId('incidents-table')).toBeVisible();
    await expect(page.getByText('Schema Lint')).toBeVisible();
    await page.locator('[data-row-key="incident-1"]').click();

    await expect(page.getByText('Confidence: 87%')).toBeVisible();
    await expect(page.getByText('High', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Resolve Incident' }).click();

    expect(resolveBody).toEqual({ reason: '' });
    await expect(page.getByText('No active incidents')).toBeVisible();
    await expect(page.getByTestId('incidents-count')).toContainText(
      '0 total incidents',
    );
  });
});
