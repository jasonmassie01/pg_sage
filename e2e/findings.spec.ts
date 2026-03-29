import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL || 'admin@localhost';
const ADMIN_PASS = process.env.E2E_ADMIN_PASSWORD || 'admin';

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
    // Wait for the severity dropdown to appear (proves initial data loaded)
    const severitySelect = page.locator('select');
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
    // Wait for the count text to appear (proves API data loaded and rendered)
    const countText = page.locator('text=/\\d+ total findings/');
    await expect(countText).toBeVisible();
  });
});
