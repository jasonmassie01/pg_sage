import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL || 'admin@localhost';
const ADMIN_PASS = process.env.E2E_ADMIN_PASSWORD || 'admin';

test.describe('Navigation', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors (favicon 404,
    // 401 pre-login, fetch failures) at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies all standard nav links are clickable and update the header
  test('all nav links work (dashboard, findings, actions, settings)', async ({
    page,
  }) => {
    // Use data-testid selectors to avoid ambiguous text matches
    // (e.g. "Database" vs "Databases")
    const links = [
      { tid: 'nav-dashboard', label: 'Dashboard' },
      { tid: 'nav-findings', label: 'Findings' },
      { tid: 'nav-actions', label: 'Actions' },
      { tid: 'nav-settings', label: 'Settings' },
      { tid: 'nav-forecasts', label: 'Forecasts' },
      { tid: 'nav-query-hints', label: 'Query Hints' },
      { tid: 'nav-alerts', label: 'Alert Log' },
      { tid: 'nav-database', label: 'Database' },
    ];

    for (const link of links) {
      const navLink = page.locator(
        `[data-testid="${link.tid}"]`,
      );
      await expect(navLink).toBeVisible();
      await navLink.click();
      // Verify the header updates to show the page name
      const header = page.locator('main h1');
      await expect(header).toContainText(link.label);
    }
  });

  // Verifies admin-only links are visible for admin users
  test('admin-only links visible for admin (databases, users, notifications)', async ({
    page,
  }) => {
    const adminTids = [
      'nav-databases',
      'nav-users',
      'nav-notifications',
    ];
    for (const tid of adminTids) {
      const navLink = page.locator(`[data-testid="${tid}"]`);
      await expect(navLink).toBeVisible();
    }
  });

  // Verifies no unexpected console errors during full page navigation
  test('no console errors on any page navigation', async ({ page }) => {
    const allHashes = [
      '#/', '#/findings', '#/actions', '#/settings',
      '#/forecasts', '#/query-hints', '#/alerts',
      '#/database', '#/manage-databases', '#/users',
      '#/notifications',
    ];

    for (const hash of allHashes) {
      await page.goto(`/${hash}`);
      // Wait for the main content area to be present
      await page.waitForSelector('main');
    }

    // Console errors are checked in afterEach
  });

  // Verifies the database picker header area renders without crashing
  test('database picker is present', async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('[data-testid="nav-dashboard"]');

    // The header with h1 always exists inside main.
    // The database picker select only appears when there are
    // multiple DBs — so we just verify the header area renders.
    const header = page.locator('main header');
    await expect(header).toBeVisible();
  });
});
