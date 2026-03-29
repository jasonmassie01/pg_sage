import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL || 'admin@localhost';
const ADMIN_PASS = process.env.E2E_ADMIN_PASSWORD || 'admin';

test.describe('Login', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors (favicon 404,
    // 401 pre-login, fetch failures) so only truly unexpected errors
    // remain in the array.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the login form renders with email, password, and submit button
  test('page loads login form', async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('form');

    const emailInput = page.locator('input[type="email"]');
    const passwordInput = page.locator('input[type="password"]');
    const submitButton = page.locator('button[type="submit"]');

    await expect(emailInput).toBeVisible();
    await expect(passwordInput).toBeVisible();
    await expect(submitButton).toBeVisible();
    await expect(submitButton).toHaveText('Sign In');
  });

  // Verifies valid credentials log in and show the dashboard nav
  test('valid login redirects to dashboard', async ({ page }) => {
    await login(page, ADMIN_EMAIL, ADMIN_PASS);

    // The nav sidebar should be visible with Dashboard link
    const dashboardLink = page.locator('nav >> text=Dashboard');
    await expect(dashboardLink).toBeVisible();

    // The header should show "Dashboard" or "pg_sage"
    const header = page.locator('main h1');
    await expect(header).toBeVisible();
  });

  // Verifies wrong credentials show an error message
  test('invalid login shows error message', async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('form');

    await page.locator('input[type="email"]').fill('bad@example.com');
    await page.locator('input[type="password"]').fill('wrongpassword');
    await page.locator('button[type="submit"]').click();

    // Wait for the error banner to appear (red-styled div inside the form)
    const errorDiv = page.locator('form >> div').filter({
      hasText: /failed|invalid|error/i,
    });
    await expect(errorDiv.first()).toBeVisible({ timeout: 10000 });
  });

  // Verifies logout clears the session and returns to login form
  test('logout clears session and shows login', async ({ page }) => {
    await login(page, ADMIN_EMAIL, ADMIN_PASS);

    // Click the Sign Out button in the sidebar
    const signOutButton = page.locator('nav >> button:has-text("Sign Out")');
    await expect(signOutButton).toBeVisible();
    await signOutButton.click();

    // Should return to the login form
    await page.waitForSelector('form');
    const emailInput = page.locator('input[type="email"]');
    await expect(emailInput).toBeVisible();
  });
});
