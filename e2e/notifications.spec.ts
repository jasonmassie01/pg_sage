import { execFileSync } from 'child_process';
import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

function sqlString(value: string): string {
  return `'${value.replace(/'/g, "''")}'`;
}

function psqlScalar(sql: string): string {
  return execFileSync('docker', [
    'exec', 'pg_sage-pg-target-1', 'psql',
    '-v', 'ON_ERROR_STOP=1',
    '-U', 'postgres',
    '-d', 'testdb',
    '-t', '-A',
    '-c', sql,
  ], { encoding: 'utf8' }).trim();
}

function cleanupChannel(name: string) {
  psqlScalar(
    `DELETE FROM sage.notification_channels WHERE name = ${sqlString(name)}`,
  );
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
        cleanupChannel(name);
      } catch (err) {
        test.skip(true, `Docker fixture database unavailable: ${err}`);
      }

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

        const storedWebhook = psqlScalar(`
          SELECT config->>'webhook_url'
            FROM sage.notification_channels
           WHERE name = ${sqlString(name)}
        `);
        const storedEnabled = psqlScalar(`
          SELECT enabled::text
            FROM sage.notification_channels
           WHERE name = ${sqlString(name)}
        `);

        expect(storedWebhook).toBe(webhook);
        expect(storedEnabled).toBe('false');
      } finally {
        cleanupChannel(name);
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
