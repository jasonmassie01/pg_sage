import { test, expect, Page } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

type Channel = {
  id: number;
  name: string;
  enabled: boolean;
  config: {
    webhook_url?: string;
  };
};

async function findChannel(page: Page, name: string): Promise<Channel | null> {
  const res = await page.request.get('/api/v1/notifications/channels');
  expect(res.status()).toBe(200);
  const data = await res.json() as { channels?: Channel[] };
  return (data.channels || []).find((ch) => ch.name === name) || null;
}

async function cleanupChannel(page: Page, name: string) {
  const channel = await findChannel(page, name);
  if (!channel) return;
  const res = await page.request.delete(
    `/api/v1/notifications/channels/${channel.id}`,
  );
  expect([200, 204, 404]).toContain(res.status());
}

test.describe('Notifications (admin)', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/notifications');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the notifications page loads with tab buttons
  test('notifications page loads with tabs', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Channels")');

    // All 3 tabs should be visible
    const channelsTab = page.locator('button:has-text("Channels")');
    await expect(channelsTab).toBeVisible();

    const rulesTab = page.locator('button:has-text("Rules")');
    await expect(rulesTab).toBeVisible();

    const logTab = page.locator('button:has-text("Log")');
    await expect(logTab).toBeVisible();
  });

  // Verifies the Channels tab shows an add form
  test('channels tab shows add form', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Channels")');

    // The channels tab (default) should have a form for adding channels
    const form = page.locator('form');
    await expect(form).toBeVisible();
  });

  test('channels preserve masked secrets when toggled from UI',
    async ({ page }, testInfo) => {
      const name = `codex-ui-secret-${testInfo.workerIndex}-${Date.now()}`;
      const webhook =
        `https://hooks.slack.com/services/T000/B000/${name}`;

      try {
        await page.locator('[data-testid="add-channel-name"]').fill(name);
        await page.locator('[data-testid="add-channel-type"]').selectOption(
          'slack',
        );
        await page.locator('[data-testid="add-channel-webhook-url"]').fill(
          webhook,
        );
        await page.locator('[data-testid="add-channel-submit"]').click();

        const row = page.locator('[data-testid="channel-row"]', {
          hasText: name,
        });
        await expect(row).toBeVisible();

        const listRes = await page.request.get(
          '/api/v1/notifications/channels',
        );
        expect(listRes.status()).toBe(200);
        const listData = await listRes.json();
        const channel = (listData.channels || []).find(
          (ch: any) => ch.name === name,
        );
        expect(channel).toBeTruthy();
        expect(channel.config.webhook_url).toContain('****');
        expect(channel.config.webhook_url).not.toBe(webhook);

        await row.locator('[data-testid="channel-toggle-button"]').click();
        await expect(row.locator('[data-testid="channel-toggle-button"]'))
          .toHaveText('OFF');

        const toggled = await findChannel(page, name);
        expect(toggled).toBeTruthy();
        expect(toggled!.config.webhook_url).toContain('****');
        expect(toggled!.config.webhook_url).not.toBe(webhook);
        expect(toggled!.enabled).toBe(false);
      } finally {
        await cleanupChannel(page, name);
      }
    });

  test('channels form exposes every backend channel type',
    async ({ page }) => {
      const typeSelect = page.locator('[data-testid="add-channel-type"]');

      await expect(typeSelect.locator('option')).toHaveText([
        'Slack',
        'Email',
        'PagerDuty',
      ]);

      await typeSelect.selectOption('pagerduty');
      await expect(page.locator('[data-testid="add-channel-routing-key"]'))
        .toBeVisible();

      await typeSelect.selectOption('email');
      await expect(page.locator('[data-testid="add-channel-smtp-host"]'))
        .toBeVisible();
      await expect(page.locator('[data-testid="add-channel-smtp-pass"]'))
        .toBeVisible();
    });

  // Verifies the Rules tab shows an add form
  test('rules tab shows add form', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Rules")');

    // Switch to Rules tab — set up response listener BEFORE click
    const rulesTab = page.locator('button:has-text("Rules")');
    await Promise.all([
      page.waitForResponse(
        (res) =>
          res.url().includes('/api/v1/notifications/rules') &&
          res.status() === 200,
      ),
      rulesTab.click(),
    ]);

    // The rules tab should have a form
    const form = page.locator('form');
    await expect(form).toBeVisible();
  });

  // Verifies the Log tab loads without errors
  test('log tab loads', async ({ page }) => {
    // Wait for tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Log")');

    // Switch to Log tab
    const logTab = page.locator('button:has-text("Log")');
    await logTab.click();

    // The log tab should render content (table or empty state)
    const mainContent = page.locator('main');
    await expect(mainContent).toBeVisible();
  });
});
