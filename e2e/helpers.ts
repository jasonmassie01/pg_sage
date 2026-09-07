import { Page } from '@playwright/test';

/**
 * Returns true if a console error message matches a known expected
 * pattern and should be ignored in afterEach assertions.
 */
export function isExpectedError(message: string, url = ''): boolean {
  const path = url ? new URL(url).pathname : '';
  if (path === '/favicon.ico' && /404/.test(message)) return true;
  return /^\/api\/v1\/auth\/(me|login)$/.test(path) && /401/.test(message);
}

/**
 * Logs in as the given user by filling the login form and waiting
 * for the app shell (nav sidebar) to appear.
 */
export async function login(
  page: Page,
  email: string,
  password: string,
): Promise<void> {
  await page.goto('/');
  // Wait for the login form to render. The email input is type="text"
  // (not type="email") so we select by data-testid, which is the stable
  // selector used across specs (see walkthrough.spec.ts).
  await page.waitForSelector('[data-testid="login-email"]');
  await page.locator('[data-testid="login-email"]').fill(email);
  await page.locator('[data-testid="login-password"]').fill(password);
  await page.locator('[data-testid="login-submit"]').click();
  // Wait for the app shell. The visible dashboard label has changed
  // over time, while App.jsx keeps this stable test id on the shell.
  await page.waitForSelector('[data-testid="app-loaded"]');
}

/**
 * Sets up a response listener for the given API path BEFORE navigating,
 * then navigates and waits for the response. This avoids the race
 * condition where the API response fires before the listener is ready.
 *
 * Use this instead of calling page.goto() then waitForAPI() separately.
 */
export async function gotoAndWaitForAPI(
  page: Page,
  url: string,
  apiPath: string,
): Promise<unknown> {
  // Set up the listener BEFORE triggering navigation
  const responsePromise = page.waitForResponse(
    (res) => res.url().includes(apiPath) && res.status() === 200,
  );
  await page.goto(url);
  const response = await responsePromise;
  return response.json();
}

/**
 * Waits for a specific API response path (e.g. '/api/v1/findings').
 * Returns the parsed JSON body.
 *
 * IMPORTANT: This must be called BEFORE the action that triggers the
 * API call, or use gotoAndWaitForAPI() for navigation + API wait.
 * If the response has already fired, this will time out.
 */
export async function waitForAPI(
  page: Page,
  path: string,
): Promise<unknown> {
  const response = await page.waitForResponse(
    (res) => res.url().includes(path) && res.status() === 200,
  );
  return response.json();
}

/**
 * Collects unexpected console errors that occur during a test.
 * Filters only favicon 404 and authentication endpoint 401 responses.
 * Failed application requests and uncaught exceptions remain observable.
 *
 * Call this in beforeEach, then assert the array is empty in afterEach.
 */
export function getConsoleErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      const text = msg.text();
      if (!isExpectedError(text, msg.location().url)) {
        errors.push(text);
      }
    }
  });
  page.on('pageerror', (error) => errors.push(error.message));
  return errors;
}

/**
 * Navigates to a hash route and waits for the page heading to update.
 */
export async function navigateTo(
  page: Page,
  hash: string,
  expectedHeading?: string,
): Promise<void> {
  await page.goto(`/#${hash}`);
  if (expectedHeading) {
    await page.waitForSelector(`h1:has-text("${expectedHeading}")`);
  }
}
