import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Settings', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    // Force Advanced mode before navigating — SettingsPage.jsx defaults to
    // 'simple' (3 tabs) but these tests cover the 7 advanced tabs.
    // Set localStorage now (same origin) then full reload so SettingsPage
    // reads the value during its getInitialMode() call.
    await page.evaluate(() => {
      window.localStorage.setItem('pg_sage_settings_mode', 'advanced');
    });
    await page.goto('/#/settings');
    await page.reload();
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the settings page loads with tab bar
  test('settings page loads with tabs', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    // All 7 tabs should be visible
    const tabs = [
      'General', 'Collector', 'Analyzer', 'Trust & Safety',
      'LLM', 'Alerting', 'Retention',
    ];
    for (const tabName of tabs) {
      const tab = page.locator(`button:has-text("${tabName}")`);
      await expect(tab).toBeVisible();
    }
  });

  // Verifies clicking each tab switches the visible content
  test('can switch between all 7 tabs', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    const tabs = [
      'General', 'Collector', 'Analyzer', 'Trust & Safety',
      'LLM', 'Alerting', 'Retention',
    ];
    for (const tabName of tabs) {
      const tab = page.locator(`button:has-text("${tabName}")`);
      await tab.click();

      // Each tab renders content inside a card (rounded div)
      const card = page.locator('div.rounded.p-5');
      await expect(card).toBeVisible();
    }
  });

  // Verifies the emergency stop button is visible on the General tab
  test('emergency stop button is visible', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    // General tab is the default, so Emergency Stop should be visible
    const emergencyBtn = page.locator(
      'button:has-text("Emergency Stop")',
    );
    await expect(emergencyBtn).toBeVisible();

    // Resume button should also be visible
    const resumeBtn = page.locator('button:has-text("Resume")');
    await expect(resumeBtn).toBeVisible();
  });

  // Verifies save/discard buttons appear when a field is modified
  test('save/discard buttons present', async ({ page }) => {
    // Wait for settings tabs to render (proves API data loaded)
    await page.waitForSelector('button:has-text("General")');

    // Switch to Collector tab which has editable fields
    const collectorTab = page.locator('button:has-text("Collector")');
    await collectorTab.click();

    // Modify a field value to trigger the save/discard buttons
    const firstInput = page.locator(
      'div.rounded.p-5 input[type="number"]',
    ).first();
    await expect(firstInput).toBeVisible();

    // Clear and type a new value to trigger the "modified" state
    await firstInput.fill('999');

    // Save and Discard buttons should now be visible. Settings uses
    // a review modal before applying changes.
    const saveBtn = page.getByTestId('settings-save');
    await expect(saveBtn).toBeVisible();
    await expect(saveBtn).toContainText('Review & Save');

    const discardBtn = page.getByTestId('settings-discard');
    await expect(discardBtn).toBeVisible();

    await saveBtn.click();
    await expect(page.getByTestId('config-diff-modal')).toBeVisible();
    await expect(page.getByTestId(
      'config-diff-row-collector.interval_seconds',
    )).toBeVisible();
    await page.getByTestId('config-diff-cancel').click();
    await expect(page.getByTestId('config-diff-modal')).toHaveCount(0);
  });

  test('global settings marks execution mode as database-only', async ({ page }) => {
    await page.waitForSelector('button:has-text("General")');
    await page.getByTestId('settings-tab-trust-safety').click();

    await expect(page.getByTestId('settings-scope')).toContainText(
      'Global defaults',
    );
    await expect(
      page.getByTestId('database-only-execution-mode'),
    ).toBeVisible();
  });

  test('global override reset restores configured default', async ({ page }) => {
    await page.waitForSelector('button:has-text("General")');
    const key = 'collector.max_queries';

    await page.evaluate(async (configKey) => {
      await fetch(`/api/v1/config/global/${encodeURIComponent(configKey)}`, {
        method: 'DELETE',
        credentials: 'include',
      });
    }, key);

    const baseline = await page.evaluate(async (configKey) => {
      const res = await fetch('/api/v1/config/global', {
        credentials: 'include',
      });
      const body = await res.json();
      return body.config[configKey];
    }, key);
    const baselineValue = Number(baseline.value);
    const overrideValue = baselineValue + 17;

    try {
      await page.evaluate(async ({ configKey, value }) => {
        const res = await fetch('/api/v1/config/global', {
          method: 'PUT',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ [configKey]: value }),
        });
        if (!res.ok) throw new Error(`override failed: ${res.status}`);
      }, { configKey: key, value: overrideValue });

      const saved = await page.evaluate(async (configKey) => {
        const res = await fetch('/api/v1/config/global', {
          credentials: 'include',
        });
        const body = await res.json();
        return body.config[configKey];
      }, key);
      expect(Number(saved.value)).toBe(overrideValue);
      expect(saved.source).toBe('override');

      await page.goto('/#/settings');
      await page.reload();
      await page.getByTestId('settings-tab-collector').click();
      await expect(page.getByTestId(`setting-${key}`)).toHaveValue(
        String(overrideValue),
      );
      await expect(page.getByTestId(`reset-${key}`)).toBeVisible();

      const deletePromise = page.waitForResponse(response =>
        response.url().includes(
          `/api/v1/config/global/${encodeURIComponent(key)}`,
        )
        && response.request().method() === 'DELETE'
        && response.status() === 200,
      );
      await page.getByTestId(`reset-${key}`).click();
      await deletePromise;

      await expect(page.getByTestId(`setting-${key}`)).toHaveValue(
        String(baselineValue),
      );
      const after = await page.evaluate(async (configKey) => {
        const res = await fetch('/api/v1/config/global', {
          credentials: 'include',
        });
        const body = await res.json();
        return body.config[configKey];
      }, key);
      expect(Number(after.value)).toBe(baselineValue);
      expect(after.source).not.toBe('override');
    } finally {
      await page.evaluate(async (configKey) => {
        await fetch(`/api/v1/config/global/${encodeURIComponent(configKey)}`, {
          method: 'DELETE',
          credentials: 'include',
        });
      }, key);
    }
  });

  test('selected database settings save execution mode per database', async ({ page }) => {
    await page.waitForSelector('button:has-text("General")');

    const fleet = await page.evaluate(async () => {
      const res = await fetch('/api/v1/databases', {
        credentials: 'include',
      });
      return res.json();
    });
    const db = (fleet.databases || []).find((d: any) =>
      (d.id || d.database_id) && d.name,
    );
    test.skip(!db, 'No managed database with id is available');
    const dbId = db.id || db.database_id;

    const before = await page.evaluate(async (id) => {
      const res = await fetch(`/api/v1/config/databases/${id}`, {
        credentials: 'include',
      });
      return res.json();
    }, dbId);
    const oldMode = before.config.execution_mode.value;
    const nextMode = oldMode === 'manual' ? 'approval' : 'manual';

    try {
      await page.getByTestId('database-picker').selectOption(db.name);
      await page.goto('/#/settings');
      await expect(page.getByTestId('settings-scope')).toContainText(
        `Database ${db.name}`,
      );

      await page.getByTestId('settings-tab-trust-safety').click();
      const selects = page.locator('div.rounded.p-5 select');
      await expect(selects).toHaveCount(2);
      await selects.nth(1).selectOption(nextMode);

      await page.getByTestId('settings-save').click();
      await expect(page.getByTestId('config-diff-modal')).toBeVisible();
      await expect(
        page.getByTestId('config-diff-row-execution_mode'),
      ).toBeVisible();
      await page.getByTestId('config-diff-confirm').click();

      await expect(page.getByTestId('config-diff-modal')).toHaveCount(0);
      const after = await page.evaluate(async (id) => {
        const res = await fetch(`/api/v1/config/databases/${id}`, {
          credentials: 'include',
        });
        return res.json();
      }, dbId);
      expect(after.config.execution_mode.value).toBe(nextMode);
    } finally {
      await page.evaluate(async ({ id, mode }) => {
        await fetch(`/api/v1/config/databases/${id}`, {
          method: 'PUT',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ execution_mode: mode }),
        });
      }, { id: dbId, mode: oldMode });
    }
  });
});
