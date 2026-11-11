import { test, expect } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { login } from './helpers';

// apiauth-03 (CHECK-16): an admin resets a viewer's password from the Users
// page; the viewer signs in with the temporary password, is held to the
// change form, and reaches the app only after choosing a new password.

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test('admin reset forces the viewer to the change form', async ({ page, browser }) => {
  const email = `reset-${randomUUID()}@test.local`;
  await login(page, ADMIN_EMAIL, ADMIN_PASS);
  const created = await page.request.post('/api/v1/users', {
    data: { email, password: `initial-${randomUUID()}`, role: 'viewer' },
  });
  expect(created.status()).toBe(201);

  await page.goto('/#/users');
  page.once('dialog', dialog => dialog.accept());
  const row = page.locator('tr', { hasText: email });
  await row.getByTestId('user-reset-password-button').click();
  const temp = (await page.getByTestId('temp-password-value').innerText()).trim();
  expect(temp).toHaveLength(32);

  const viewer = await (await browser.newContext()).newPage();
  await viewer.goto('/');
  await viewer.getByTestId('login-email').fill(email);
  await viewer.getByTestId('login-password').fill(temp);
  await viewer.getByTestId('login-submit').click();
  await expect(viewer.getByTestId('forced-password-change')).toBeVisible();
  expect((await viewer.request.get('/api/v1/cases')).status()).toBe(403);

  const chosen = `chosen-${randomUUID()}`;
  await viewer.getByTestId('current-password').fill(temp);
  await viewer.getByTestId('new-password').fill(chosen);
  await viewer.getByTestId('confirm-password').fill(chosen);
  await viewer.getByTestId('change-password-submit').click();
  await viewer.waitForSelector('[data-testid="app-loaded"]');
  expect((await viewer.request.get('/api/v1/cases')).status()).not.toBe(403);

  const cleanup = await page.request.get('/api/v1/users');
  const users = (await cleanup.json()).users as { id: number; email: string }[];
  const id = users.find(u => u.email === email)?.id;
  if (id) await page.request.delete(`/api/v1/users/${id}`);
});
