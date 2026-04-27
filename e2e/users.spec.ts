import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Users (admin)', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/users');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the users management page loads for admin users
  test('users page loads (admin only)', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('h2:has-text("Add User")');

    // The "Add User" heading should be visible
    const addHeading = page.locator('h2:has-text("Add User")');
    await expect(addHeading).toBeVisible();
  });

  // Verifies the add user form has email, password, and role fields
  test('add user form has email, password, role fields', async ({
    page,
  }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('h2:has-text("Add User")');

    // Email input
    const emailLabel = page.locator('label:has-text("Email")');
    await expect(emailLabel).toBeVisible();
    const emailInput = page.locator(
      'form >> input[type="email"]',
    );
    await expect(emailInput).toBeVisible();

    // Password input
    const passwordLabel = page.locator('label:has-text("Password")');
    await expect(passwordLabel).toBeVisible();
    const passwordInput = page.locator(
      'form >> input[type="password"]',
    );
    await expect(passwordInput).toBeVisible();

    // Role select
    const roleLabel = page.locator('label:has-text("Role")');
    await expect(roleLabel).toBeVisible();
    const roleSelect = page.locator('form >> select');
    await expect(roleSelect).toBeVisible();

    // Verify role options exist
    const options = roleSelect.locator('option');
    const optionTexts = await options.allTextContents();
    expect(optionTexts).toContain('viewer');
    expect(optionTexts).toContain('operator');
    expect(optionTexts).toContain('admin');
  });

  // Verifies the users table renders with column headers
  test('users table displays', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('table');

    // The table should be present with expected column headers
    const table = page.locator('table');
    await expect(table).toBeVisible();

    const headers = table.locator('th');
    const headerTexts = await headers.allTextContents();
    expect(headerTexts).toContain('Email');
    expect(headerTexts).toContain('Role');
    expect(headerTexts).toContain('Created');
    expect(headerTexts).toContain('Last Login');
  });
});
