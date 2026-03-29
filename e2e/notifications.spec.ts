import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL || 'admin@localhost';
const ADMIN_PASS = process.env.E2E_ADMIN_PASSWORD || 'admin';

test.describe('Notifications (admin)', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/notifications');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the notifications page loads with tab buttons
  test('notifications page loads with tabs', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Channels")');

    // All 3 tabs should be visible
    const channelsTab = page.locator('button:has-text("Channels")');
    await expect(channelsTab).toBeVisible();

    const rulesTab = page.locator('button:has-text("Rules")');
    await expect(rulesTab).toBeVisible();

    const logTab = page.locator('button:has-text("Log")');
    await expect(logTab).toBeVisible();
  });

  // Verifies the Channels tab shows an add form
  test('channels tab shows add form', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Channels")');

    // The channels tab (default) should have a form for adding channels
    const form = page.locator('form');
    await expect(form).toBeVisible();
  });

  // Verifies the Rules tab shows an add form
  test('rules tab shows add form', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Rules")');

    // Switch to Rules tab — set up response listener BEFORE click
    const rulesTab = page.locator('button:has-text("Rules")');
    await Promise.all([
      page.waitForResponse(
        (res) =>
          res.url().includes('/api/v1/notifications/rules') &&
          res.status() === 200,
      ),
      rulesTab.click(),
    ]);

    // The rules tab should have a form
    const form = page.locator('form');
    await expect(form).toBeVisible();
  });

  // Verifies the Log tab loads without errors
  test('log tab loads', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Log")');

    // Switch to Log tab
    const logTab = page.locator('button:has-text("Log")');
    await logTab.click();

    // The log tab should render content (table or empty state)
    const mainContent = page.locator('main');
    await expect(mainContent).toBeVisible();
  });
});
