import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL || 'admin@localhost';
const ADMIN_PASS = process.env.E2E_ADMIN_PASSWORD || 'admin';

test.describe('Databases (admin)', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/manage-databases');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the databases management page loads for admin users
  test('databases page loads (admin only)', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    // The "Add Database" button should be visible
    const addBtn = page.locator('button:has-text("Add Database")');
    await expect(addBtn).toBeVisible();
  });

  // Verifies clicking "Add Database" opens the database form
  test('add database button opens form', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    const addBtn = page.locator('button:has-text("Add Database")');
    await addBtn.click();

    // The form should appear with a heading "Add Database"
    const formHeading = page.locator('h2:has-text("Add Database")');
    await expect(formHeading).toBeVisible();
  });

  // Verifies the database form contains all required connection fields
  test('database form has all required fields', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    const addBtn = page.locator('button:has-text("Add Database")');
    await addBtn.click();

    // Check for all field labels (use exact text match to avoid
    // "Name" matching "Username")
    const requiredLabels = [
      'Name', 'Host', 'Port', 'Database', 'Username',
      'Password', 'SSL Mode', 'Trust Level', 'Execution Mode',
    ];
    for (const label of requiredLabels) {
      const labelEl = page.locator('label').filter({
        hasText: new RegExp(`^${label}$`),
      });
      await expect(labelEl).toBeVisible();
    }

    // Save and Cancel buttons should be present
    const saveBtn = page.locator(
      'form >> button[type="submit"]:has-text("Save")',
    );
    await expect(saveBtn).toBeVisible();

    const cancelBtn = page.locator(
      'form >> button:has-text("Cancel")',
    );
    await expect(cancelBtn).toBeVisible();
  });

  // Verifies the Cancel button closes the form
  test('cancel button closes form', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    const addBtn = page.locator('button:has-text("Add Database")');
    await addBtn.click();

    // Confirm the form is open
    const formHeading = page.locator('h2:has-text("Add Database")');
    await expect(formHeading).toBeVisible();

    // Click Cancel
    const cancelBtn = page.locator('form >> button:has-text("Cancel")');
    await cancelBtn.click();

    // The form heading should no longer be visible
    await expect(formHeading).not.toBeVisible();
  });
});
