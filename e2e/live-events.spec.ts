import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';
import { seedAction } from './action-fixtures';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe.serial('Live events', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('updates the pending actions badge from action SSE events', async ({
    page,
  }) => {
    const queueID = seedAction('events');
    await page.goto('/#/');

    const setup = await page.evaluate(async (queueID) => {
      const countRes = await fetch('/api/v1/actions/pending/count', {
        credentials: 'include',
      });
      const beforeCount = (await countRes.json()).count || 0;
      const pendingRes = await fetch('/api/v1/actions/pending', {
        credentials: 'include',
      });
      const pending = (await pendingRes.json()).pending || [];
      const target = pending.find((a) =>
        a.database_name === 'testdb' && a.id === queueID
      );
      return { beforeCount, target };
    }, queueID);

    expect(setup.target, 'seeded event action must be returned').toBeTruthy();
    expect(setup.beforeCount).toBeGreaterThan(0);

    const actionsNav = page.getByTestId('nav-actions');
    const initialBadge = actionsNav.locator('span');
    await expect(initialBadge).toHaveText(String(setup.beforeCount));

    await page.evaluate(async (target) => {
      const res = await fetch(
        `/api/v1/actions/${target.id}/reject?database=${
          encodeURIComponent(target.database_name)
        }`,
        {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ reason: 'e2e live badge refresh' }),
        },
      );
      if (!res.ok) {
        throw new Error(`reject failed: ${res.status}`);
      }
    }, setup.target);

    const afterCount = setup.beforeCount - 1;
    if (afterCount > 0) {
      await expect(initialBadge).toHaveText(String(afterCount), {
        timeout: 10000,
      });
    } else {
      await expect(initialBadge).toHaveCount(0, { timeout: 10000 });
    }
  });
});
