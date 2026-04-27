import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';
const TEST_PASS = 'CodexRole123!';

async function createUser(page: any, email: string, role: string) {
  const res = await page.request.post('/api/v1/users', {
    data: { email, password: TEST_PASS, role },
  });
  expect([201, 409]).toContain(res.status());
}

async function logout(page: any) {
  await page.request.post('/api/v1/auth/logout', { data: {} });
  await page.context().clearCookies();
}

test.describe('Role boundaries', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors.filter((msg) => !msg.includes('403')))
      .toEqual([]);
  });

  test('viewer can read executed actions but cannot access pending review UI', async ({
    page,
  }) => {
    const email = `codex-viewer-${Date.now()}@pg-sage.local`;
    await createUser(page, email, 'viewer');
    await logout(page);
    await login(page, email, TEST_PASS);

    await page.goto('/#/actions');
    await expect(page.getByTestId('actions-tab-executed')).toBeVisible();
    await expect(page.getByTestId('actions-tab-pending')).toHaveCount(0);
    await expect(page.getByTestId('nav-users')).toHaveCount(0);
    await expect(page.getByTestId('nav-databases')).toHaveCount(0);
    await expect(page.getByTestId('nav-notifications')).toHaveCount(0);

    const pending = await page.request.get('/api/v1/actions/pending');
    expect(pending.status()).toBe(403);
  });

  test('operator can access pending review but not admin management', async ({
    page,
  }) => {
    const email = `codex-operator-${Date.now()}@pg-sage.local`;
    await createUser(page, email, 'operator');
    await logout(page);
    await login(page, email, TEST_PASS);

    await page.goto('/#/actions');
    await expect(page.getByTestId('actions-tab-pending')).toBeVisible();
    const pending = await page.request.get('/api/v1/actions/pending');
    expect(pending.status()).toBe(200);

    await expect(page.getByTestId('nav-users')).toHaveCount(0);
    const users = await page.request.get('/api/v1/users');
    expect(users.status()).toBe(403);
  });
});
