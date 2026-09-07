import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const PASSWORD = process.env.PG_SAGE_ADMIN_PASS || 'admin';

// Navigation does not create cloud resources or require provider credentials.
test('operator can navigate Agent DB provider and provisioning panels', async ({ page }) => {
  const errors = getConsoleErrors(page);
  await login(page, EMAIL, PASSWORD);
  await page.getByTestId('nav-agent-dbs').click();
  await expect(page.getByTestId('agent-dbs-page')).toBeVisible();
  for (const name of ['Provider Settings', 'Terraform', 'Provision']) {
    const tab = page.getByRole('tab', { name, exact: true });
    await tab.click();
    await expect(tab).toHaveAttribute('aria-selected', 'true');
    const panelID = await tab.getAttribute('aria-controls');
    expect(panelID).toBeTruthy();
    await expect(page.locator(`[id="${panelID}"]`)).toBeVisible();
  }
  expect(errors).toEqual([]);
});
