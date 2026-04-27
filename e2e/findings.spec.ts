import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Findings', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/findings');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the findings page loads with status filter tabs and content
  test('findings page loads with table', async ({ page }) => {
    // Wait for the status filter buttons to appear (proves API data loaded)
    const openBtn = page.locator('button:has-text("open")');
    await expect(openBtn).toBeVisible();

    const suppressedBtn = page.locator('button:has-text("suppressed")');
    await expect(suppressedBtn).toBeVisible();

    const resolvedBtn = page.locator('button:has-text("resolved")');
    await expect(resolvedBtn).toBeVisible();

    // The page should show either a data table or an empty state message
    const mainContent = page.locator('main');
    await expect(mainContent).toBeVisible();
  });

  // Verifies the severity filter dropdown changes the API call
  test('severity filter changes displayed findings', async ({ page }) => {
    // Use the stable testid — the Layout header may render a DatabasePicker
    // <select> as well, which would make a bare `select` locator ambiguous.
    const severitySelect = page.locator('[data-testid="severity-filter"]');
    await expect(severitySelect).toBeVisible();

    // Change to "critical" and wait for the filtered API response
    const [response] = await Promise.all([
      page.waitForResponse(
        (res) =>
          res.url().includes('/api/v1/findings') &&
          res.url().includes('severity=critical'),
        { timeout: 10000 },
      ),
      severitySelect.selectOption('critical'),
    ]);
    expect(response.status()).toBe(200);
  });

  // Verifies the total findings count is displayed at the bottom
  test('findings count is displayed', async ({ page }) => {
    // Findings.jsx:118-122 renders "<n> total recommendations" with this testid
    const countText = page.locator('[data-testid="findings-count"]');
    await expect(countText).toBeVisible();
    await expect(countText).toHaveText(/\d+ total recommendations/);
  });

  test('pending approval findings do not show manual Take Action', async ({
    page,
  }) => {
    const target = await page.evaluate(async () => {
      const pendingRes = await fetch('/api/v1/actions/pending', {
        credentials: 'include',
      });
      const pendingJson = await pendingRes.json();
      const pending = pendingJson.pending || [];
      const preferred = [
        ...pending.filter((a) => a.database_name === 'testdb2'),
        ...pending,
      ];
      const seen = new Set();
      for (const action of preferred) {
        if (seen.has(action.database_name)) continue;
        seen.add(action.database_name);
        const findingsRes = await fetch(
          `/api/v1/findings?status=open&database=${
            encodeURIComponent(action.database_name)
          }&limit=50`,
          { credentials: 'include' },
        );
        const findingsJson = await findingsRes.json();
        const finding = (findingsJson.findings || [])
          .find((f) => Number(f.id) === Number(action.finding_id));
        if (finding) {
          return {
            title: finding.title,
            database: finding.database_name,
          };
        }
      }
      return null;
    });
    expect(target, 'expected visible finding with pending action').toBeTruthy();

    await page.getByTestId('database-picker').selectOption(target!.database);

    const row = page.locator('tbody tr')
      .filter({ hasText: target!.title })
      .filter({ hasText: target!.database });
    await expect(row).toHaveCount(1);
    await row.click();

    await expect(page.getByTestId('pending-action-panel')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Take Action' }))
      .toHaveCount(0);
  });

  test('suppresses and unsuppresses a finding from the UI', async ({
    page,
  }) => {
    const target = await page.evaluate(async () => {
      const dbsRes = await fetch('/api/v1/databases', {
        credentials: 'include',
      });
      const dbsJson = await dbsRes.json();
      const names = (dbsJson.databases || []).map((d) => d.name);
      const preferred = [
        ...names.filter((n) => n === 'testdb2'),
        ...names.filter((n) => n !== 'testdb2'),
      ];

      for (const database of preferred) {
        const res = await fetch(
          `/api/v1/findings?status=open&database=${
            encodeURIComponent(database)
          }&limit=50`,
          { credentials: 'include' },
        );
        const json = await res.json();
        const findings = json.findings || [];
        const counts = findings.reduce((acc, f) => {
          const key = `${f.title}|${f.category}|${f.database_name}`;
          acc[key] = (acc[key] || 0) + 1;
          return acc;
        }, {});
        const finding = findings.find((f) =>
          f.category === 'schema_lint:lint_no_primary_key' &&
          counts[`${f.title}|${f.category}|${f.database_name}`] === 1
        ) || findings.find((f) =>
          f.category.startsWith('schema_lint:') &&
          counts[`${f.title}|${f.category}|${f.database_name}`] === 1
        );
        if (finding) {
          return {
            id: Number(finding.id),
            title: finding.title,
            category: finding.category,
            database: finding.database_name,
          };
        }
      }
      return null;
    });
    expect(target, 'expected suppressible open finding').toBeTruthy();

    await page.getByTestId('database-picker').selectOption(target!.database);

    const openRow = page.locator(`tbody tr[data-row-key="${target!.id}"]`);
    await expect(openRow).toHaveCount(1);
    await openRow.click();

    await page.getByTestId('suppress-button').click();
    await expect(page.getByTestId('suppress-confirm-modal')).toBeVisible();

    const suppressResponse = page.waitForResponse((res) =>
      res.url().includes(`/api/v1/findings/${target!.id}/suppress`) &&
      res.status() === 200,
    );
    await page.getByTestId('suppress-confirm').click();
    const suppressJson = await (await suppressResponse).json();
    expect(suppressJson.ok).toBe(true);
    expect(suppressJson.status).toBe('suppressed');

    await expect(openRow).toHaveCount(0);

    const suppressedJson = await page.evaluate(async ({ id, database }) => {
      const res = await fetch(
        `/api/v1/findings?status=suppressed&database=${
          encodeURIComponent(database)
        }&limit=50`,
        { credentials: 'include' },
      );
      return res.json().then((json) =>
        (json.findings || []).find((f) => Number(f.id) === id) || null
      );
    }, target);
    expect(suppressedJson).toBeTruthy();

    const suppressedListResponse = page.waitForResponse((res) =>
      res.url().includes('/api/v1/findings') &&
      res.url().includes('status=suppressed') &&
      res.url().includes(`database=${encodeURIComponent(target!.database)}`) &&
      res.status() === 200,
    );
    await page.getByRole('button', { name: 'Suppressed' }).click();
    await suppressedListResponse;
    const suppressedRow = page.locator(
      `tbody tr[data-row-key="${target!.id}"]`,
    );
    await expect(suppressedRow).toHaveCount(1);
    if (await page.getByTestId('suppress-button').count() === 0) {
      await suppressedRow.click();
    }
    await expect(page.getByTestId('suppress-button'))
      .toHaveText('Unsuppress');

    const unsuppressResponse = page.waitForResponse((res) =>
      res.url().includes(`/api/v1/findings/${target!.id}/unsuppress`) &&
      res.status() === 200,
    );
    await page.getByTestId('suppress-button').click();
    const unsuppressJson = await (await unsuppressResponse).json();
    expect(unsuppressJson.ok).toBe(true);
    expect(unsuppressJson.status).toBe('open');

    await page.getByRole('button', { name: 'Open' }).click();
    await expect(openRow).toHaveCount(1);
  });
});
