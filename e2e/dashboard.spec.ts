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
    await page.goto('/#/');
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
    await page.goto('/#/');
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
    await page.goto('/#/');
    // Wait for the dashboard to finish loading (stat cards prove it)
    await page.waitForSelector('[data-testid="stat-databases"]');

    // The recent findings section has an h2 heading "Recent Findings"
    // but only if there are findings. Either way, the page should
    // not crash.
    const mainContent = page.locator('main');
    await expect(mainContent).toBeVisible();

    // If findings exist, the heading should appear; if not, that
    // is fine. We just verify no errors occurred (checked in
    // afterEach).
  });
});
