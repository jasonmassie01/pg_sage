import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Actions', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/actions');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the actions page loads with tab buttons
  test('actions page loads with tabs', async ({ page }) => {
    // Wait for the tab buttons to appear (proves API data loaded)
    const executedTab = page.locator('button:has-text("Executed")');
    await expect(executedTab).toBeVisible();

    const pendingTab = page.locator('button:has-text("Pending Approval")');
    await expect(pendingTab).toBeVisible();
  });

  // Verifies switching between executed and pending tabs works
  test('can switch between executed and pending tabs', async ({ page }) => {
    // Wait for the tab buttons to appear (proves initial data loaded)
    const pendingTab = page.locator('button:has-text("Pending Approval")');
    await expect(pendingTab).toBeVisible();

    // Click the pending tab
    await pendingTab.click();

    // The pending tab should now be styled as active (has accent background)
    // and the page should show either a table or an empty state
    const mainContent = page.locator('main');
    await expect(mainContent).toBeVisible();

    // Switch back to executed
    const executedTab = page.locator('button:has-text("Executed")');
    await executedTab.click();
    await expect(mainContent).toBeVisible();
  });

  // Verifies pending tab shows approve/reject buttons when pending actions exist
  test('pending tab shows approve/reject buttons (if pending actions exist)', async ({
    page,
  }) => {
    // Wait for the tab buttons to appear (proves initial data loaded)
    const pendingTab = page.locator('button:has-text("Pending Approval")');
    await expect(pendingTab).toBeVisible();

    // Click the pending tab
    await pendingTab.click();

    // Wait a moment for the tab content to render
    await page.waitForTimeout(1000);

    // If there are pending actions, Approve buttons appear (testid from
    // Actions.jsx:345). If there are none, Actions.jsx:370 renders an
    // EmptyState with "No actions waiting for approval...".
    const approveButtons = page.locator('[data-testid="approve-button"]');
    const emptyState = page.locator('text=/No actions waiting/i');

    const approveCount = await approveButtons.count();
    const emptyCount = await emptyState.count();

    // One of these must be true: either pending actions or empty state
    expect(approveCount > 0 || emptyCount > 0).toBe(true);
  });
});
