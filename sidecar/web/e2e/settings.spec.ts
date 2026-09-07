// settings.spec.ts — Smoke tests for the Settings page.
//
// Verifies simple/advanced mode toggle, tab navigation,
// config field rendering, save button gating, and emergency
// stop controls.

import { test, expect } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

test.describe('Settings page', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
    // Clear localStorage so we always start in simple mode.
    await page.addInitScript(() => {
      localStorage.removeItem('pg_sage_settings_mode')
    })
  })

  test('loads in simple mode with correct tabs', async ({ page }) => {
    await page.goto('#/settings')

    // Simple mode tabs: General, Monitoring, AI & Alerts
    await expect(
      page.locator('[data-testid="settings-tab-general"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-monitoring"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-ai-alerts"]'),
    ).toBeVisible()

    // Advanced-only tabs should NOT be visible.
    await expect(
      page.locator('[data-testid="settings-tab-collector"]'),
    ).not.toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-retention"]'),
    ).not.toBeVisible()
  })

  test('mode toggle switches to advanced with more tabs', async ({
    page,
  }) => {
    await page.goto('#/settings')

    const toggle = page.locator('[data-testid="settings-mode-toggle"]')
    await expect(toggle).toBeVisible()
    await expect(toggle).toContainText('Show Advanced')

    await toggle.click()
    await expect(toggle).toContainText('Show Simple')

    // Advanced tabs should now appear.
    await expect(
      page.locator('[data-testid="settings-tab-collector"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-analyzer"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-trust-safety"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-llm"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-alerting"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-tab-retention"]'),
    ).toBeVisible()
  })

  test('General tab shows system info and emergency controls', async ({
    page,
  }) => {
    await page.goto('#/settings')

    // General tab is active by default — shows system info.
    await expect(page.getByText('System Info')).toBeVisible()
    await expect(page.getByText('Mode', { exact: true })).toBeVisible()
    await expect(page.getByText('fleet', { exact: true })).toBeVisible()

    // Emergency controls
    const stopBtn = page.locator(
      '[data-testid="emergency-stop-button"]',
    )
    await expect(stopBtn).toBeVisible()
    await expect(stopBtn).toContainText('Emergency Stop')

    const resumeBtn = page.locator('[data-testid="resume-button"]')
    await expect(resumeBtn).toBeVisible()
    await expect(resumeBtn).toContainText('Resume')
  })

  test('tab navigation shows different content', async ({ page }) => {
    await page.goto('#/settings')

    // Switch to Monitoring tab.
    await page.locator(
      '[data-testid="settings-tab-monitoring"]',
    ).click()
    await expect(
      page.getByText('How pg_sage monitors'),
    ).toBeVisible()
    await expect(
      page.getByText('Collector Interval (seconds)'),
    ).toBeVisible()

    // Switch to AI & Alerts tab.
    await page.locator(
      '[data-testid="settings-tab-ai-alerts"]',
    ).click()
    await expect(page.getByText('AI Analysis')).toBeVisible()
    await expect(page.getByText('LLM Enabled')).toBeVisible()
  })

  test('config fields display current values', async ({ page }) => {
    await page.goto('#/settings')

    // Go to Monitoring tab where numeric fields are.
    await page.locator(
      '[data-testid="settings-tab-monitoring"]',
    ).click()

    // The collector interval field should show 60 from our mock.
    const intervalInput = page.locator(
      'input[type="number"]',
    ).first()
    await expect(intervalInput).toBeVisible()
    await expect(intervalInput).toHaveValue('60')
  })

  test('save button appears only after edits', async ({ page }) => {
    await page.goto('#/settings')

    // On General tab, there is no save button (it is readonly info).
    await expect(
      page.locator('[data-testid="settings-save"]'),
    ).not.toBeVisible()

    // Switch to Monitoring tab.
    await page.locator(
      '[data-testid="settings-tab-monitoring"]',
    ).click()

    // No edits yet — save should not be visible.
    await expect(
      page.locator('[data-testid="settings-save"]'),
    ).not.toBeVisible()

    // Make an edit by changing the first number input.
    const input = page.locator('input[type="number"]').first()
    await input.fill('120')

    // Save button should now appear.
    await expect(
      page.locator('[data-testid="settings-save"]'),
    ).toBeVisible()
    await expect(
      page.locator('[data-testid="settings-save"]'),
    ).toContainText('Review & Save')

    // Discard button should also be present.
    await expect(
      page.locator('[data-testid="settings-discard"]'),
    ).toBeVisible()
  })

  test('advanced mode tab navigation works', async ({ page }) => {
    await page.goto('#/settings')

    // Switch to advanced mode.
    await page.locator(
      '[data-testid="settings-mode-toggle"]',
    ).click()

    // Click through each advanced tab.
    await page.locator(
      '[data-testid="settings-tab-collector"]',
    ).click()
    await expect(page.getByText('Interval (seconds)')).toBeVisible()

    await page.locator(
      '[data-testid="settings-tab-analyzer"]',
    ).click()
    await expect(
      page.getByText('Slow Query Threshold (ms)'),
    ).toBeVisible()

    await page.locator(
      '[data-testid="settings-tab-trust-safety"]',
    ).click()
    await expect(page.getByText('Trust Level')).toBeVisible()
    await expect(page.getByText('CPU Ceiling (%)')).toBeVisible()

    await page.locator('[data-testid="settings-tab-llm"]').click()
    await expect(page.getByText('Token Budget (daily)')).toBeVisible()

    await page.locator(
      '[data-testid="settings-tab-alerting"]',
    ).click()
    await expect(
      page.getByText('Slack Webhook URL'),
    ).toBeVisible()

    await page.locator(
      '[data-testid="settings-tab-retention"]',
    ).click()
    await expect(
      page.getByText('Snapshots (days)'),
    ).toBeVisible()
  })

  test('discard button clears edits', async ({ page }) => {
    await page.goto('#/settings')

    await page.locator(
      '[data-testid="settings-tab-monitoring"]',
    ).click()

    const input = page.locator('input[type="number"]').first()
    const original = await input.inputValue()
    await input.fill('999')

    // Save button is visible — edits exist.
    await expect(
      page.locator('[data-testid="settings-save"]'),
    ).toBeVisible()

    // Click discard.
    await page.locator('[data-testid="settings-discard"]').click()

    // Save button should disappear and value should revert.
    await expect(
      page.locator('[data-testid="settings-save"]'),
    ).not.toBeVisible()
    await expect(input).toHaveValue(original)
  })
})
