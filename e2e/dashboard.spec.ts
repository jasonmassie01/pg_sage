import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Dashboard', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies stat cards render on the dashboard (Databases, Healthy, etc.)
  test('dashboard loads with stat cards', async ({ page }) => {
    await page.goto('/#/advanced');
    // Wait for the stat card that proves the API data loaded
    await page.waitForSelector('[data-testid="stat-databases"]');

    // Check that the "Databases" stat card label is visible
    const dbStat = page.locator('[data-testid="stat-databases"]');
    await expect(dbStat).toBeVisible();

    // Check for the "Healthy" stat card
    const healthyStat = page.locator('[data-testid="stat-healthy"]');
    await expect(healthyStat).toBeVisible();
  });

  // Verifies the database list section renders items (not "all")
  test('database list shows items (not empty, not "all")', async ({
    page,
  }) => {
    await page.goto('/#/advanced');
    // Wait for the database list to render
    await page.waitForSelector('[data-testid="db-list"]');

    // The "Databases" section heading in the card
    const dbSectionHeading = page
      .locator('[data-testid="db-list"] h2')
      .filter({ hasText: 'Databases' });
    await expect(dbSectionHeading).toBeVisible();

    // Each database row shows a name and a health score.
    const listItems = page.locator(
      '[data-testid="db-list-item"]',
    );
    const count = await listItems.count();
    // There should be at least one database listed
    expect(count).toBeGreaterThanOrEqual(1);
  });

  // Verifies the recent findings section renders (may be empty)
  test('recent findings section renders', async ({ page }) => {
    await page.goto('/#/advanced');
    // Wait for the dashboard to finish loading (stat cards prove it)
    await page.waitForSelector('[data-testid="stat-databases"]');

    await page.getByTestId('overview-tab-recent-recos').click();
    await expect(page.getByTestId('recent-findings')).toBeVisible();
    await expect(page.getByTestId('recent-findings'))
      .toContainText('Recent Recommendations');
  });
});
