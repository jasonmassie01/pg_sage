import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

async function fleetSummary(page) {
  return page.evaluate(async () => {
    const res = await fetch('/api/v1/databases', {
      credentials: 'include',
    });
    if (!res.ok) throw new Error(`databases failed: ${res.status}`);
    return res.json();
  });
}

function latestAnalyzerRun(data) {
  return Math.max(...(data.databases || []).map((db) =>
    Date.parse(db.status?.analyzer_last_run || '') || 0
  ));
}

test.describe.serial('Emergency stop', () => {
  test.setTimeout(120000);

  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('stops actions, resumes, and monitoring continues afterward', async ({
    page,
  }) => {
    const before = await fleetSummary(page);
    const beforeAnalyzer = latestAnalyzerRun(before);
    expect(before.summary.total_databases).toBeGreaterThan(0);

    await page.goto('/#/settings');
    await page.getByTestId('emergency-stop-button').click();
    await expect(page.getByTestId('emergency-stop-button'))
      .toContainText('Confirm Emergency Stop');

    const stopResponse = page.waitForResponse((res) =>
      res.url().includes('/api/v1/emergency-stop') &&
      res.status() === 200,
    );
    await page.getByTestId('emergency-stop-button').click();
    await stopResponse;

    await expect.poll(async () => {
      const data = await fleetSummary(page);
      return data.summary.emergency_stopped;
    }, { timeout: 10000 }).toBe(true);

    const resumeResponse = page.waitForResponse((res) =>
      res.url().includes('/api/v1/resume') &&
      res.status() === 200,
    );
    await page.getByTestId('resume-button').click();
    await resumeResponse;

    await expect.poll(async () => {
      const data = await fleetSummary(page);
      return data.summary.emergency_stopped;
    }, { timeout: 10000 }).toBe(false);

    await expect.poll(async () => {
      const data = await fleetSummary(page);
      return latestAnalyzerRun(data);
    }, {
      timeout: 90000,
      intervals: [5000, 10000, 15000, 20000],
    }).toBeGreaterThan(beforeAnalyzer);
  });
});
