import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

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
    await page.waitForSelector('[data-testid="login-email"]');

    const emailInput = page.locator('[data-testid="login-email"]');
    const passwordInput = page.locator('[data-testid="login-password"]');
    const submitButton = page.locator('[data-testid="login-submit"]');

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
    await page.waitForSelector('[data-testid="login-email"]');

    await page.locator('[data-testid="login-email"]').fill('bad@example.com');
    await page.locator('[data-testid="login-password"]').fill('wrongpassword');
    await page.locator('[data-testid="login-submit"]').click();

    // Wait for the error banner to appear — LoginPage.jsx emits a div with
    // data-testid="login-error" when the /api/v1/auth/login call fails.
    const errorBanner = page.locator('[data-testid="login-error"]');
    await expect(errorBanner).toBeVisible({ timeout: 10000 });
  });

  // Verifies logout clears the session and returns to login form
  test('logout clears session and shows login', async ({ page }) => {
    await login(page, ADMIN_EMAIL, ADMIN_PASS);

    // Click the Sign Out button in the sidebar (stable testid)
    const signOutButton = page.locator('[data-testid="sign-out-button"]');
    await expect(signOutButton).toBeVisible();
    await signOutButton.click();

    // Should return to the login form
    await page.waitForSelector('[data-testid="login-email"]');
    const emailInput = page.locator('[data-testid="login-email"]');
    await expect(emailInput).toBeVisible();
  });
});
