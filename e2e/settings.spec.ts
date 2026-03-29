import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL || 'admin@localhost';
const ADMIN_PASS = process.env.E2E_ADMIN_PASSWORD || 'admin';

test.describe('Settings', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/settings');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the settings page loads with tab bar
  test('settings page loads with tabs', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    // All 7 tabs should be visible
    const tabs = [
      'General', 'Collector', 'Analyzer', 'Trust & Safety',
      'LLM', 'Alerting', 'Retention',
    ];
    for (const tabName of tabs) {
      const tab = page.locator(`button:has-text("${tabName}")`);
      await expect(tab).toBeVisible();
    }
  });

  // Verifies clicking each tab switches the visible content
  test('can switch between all 7 tabs', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    const tabs = [
      'General', 'Collector', 'Analyzer', 'Trust & Safety',
      'LLM', 'Alerting', 'Retention',
    ];
    for (const tabName of tabs) {
      const tab = page.locator(`button:has-text("${tabName}")`);
      await tab.click();

      // Each tab renders content inside a card (rounded div)
      const card = page.locator('div.rounded.p-5');
      await expect(card).toBeVisible();
    }
  });

  // Verifies the emergency stop button is visible on the General tab
  test('emergency stop button is visible', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    // General tab is the default, so Emergency Stop should be visible
    const emergencyBtn = page.locator(
      'button:has-text("Emergency Stop")',
    );
    await expect(emergencyBtn).toBeVisible();

    // Resume button should also be visible
    const resumeBtn = page.locator('button:has-text("Resume")');
    await expect(resumeBtn).toBeVisible();
  });

  // Verifies save/discard buttons appear when a field is modified
  test('save/discard buttons present', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    // Switch to Collector tab which has editable fields
    const collectorTab = page.locator('button:has-text("Collector")');
    await collectorTab.click();

    // Modify a field value to trigger the save/discard buttons
    const firstInput = page.locator(
      'div.rounded.p-5 input[type="number"]',
    ).first();
    await expect(firstInput).toBeVisible();

    // Clear and type a new value to trigger the "modified" state
    await firstInput.fill('999');

    // Save and Discard buttons should now be visible
    const saveBtn = page.locator('button:has-text("Save Changes")');
    await expect(saveBtn).toBeVisible();

    const discardBtn = page.locator('button:has-text("Discard")');
    await expect(discardBtn).toBeVisible();
  });
});
