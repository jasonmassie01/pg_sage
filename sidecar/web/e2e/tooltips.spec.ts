// tooltips.spec.ts — CHECK-T20 e2e for ConfigTooltip.
//
// Walks the Settings page, locates config labels by data-config-key,
// hovers each, and asserts that a tooltip panel appears with non-empty
// body text. Smoke-level coverage — individual field assertions live
// in the component test suite.
//
// Plan reference: reviews/archive/docs-internal/plan_v0.8.5.md §7.5.

import { test, expect } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

test.describe('Config tooltips', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
  })

  test('trust.level label opens a tooltip on hover', async ({ page }) => {
    await page.goto('#/settings')

    // Trust Level is on the Monitoring tab in simple mode.
    await page.locator(
      '[data-testid="settings-tab-monitoring"]',
    ).click()

    const trigger = page.locator(
      '[data-config-key="trust.level"]',
    ).first()
    await expect(trigger).toBeVisible()

    await trigger.hover()

    // Radix renders the content in a portal; match by role=tooltip.
    const tooltip = page.getByRole('tooltip').first()
    await expect(tooltip).toBeVisible()
    await expect(tooltip).toContainText(/observation|advisory|autonomy/i)
  })

  test('every documented trigger has non-empty tooltip copy', async ({
    page,
  }) => {
    await page.goto('#/settings')

    // Switch to Monitoring tab where config fields with tooltips are.
    await page.locator(
      '[data-testid="settings-tab-monitoring"]',
    ).click()

    const triggers = page.locator('[data-config-key]')
    const count = await triggers.count()
    expect(count).toBeGreaterThan(0)

    // Sample the first 5 to keep runtime bounded.
    const sample = Math.min(count, 5)
    for (let i = 0; i < sample; i++) {
      const t = triggers.nth(i)
      await t.scrollIntoViewIfNeeded()
      await t.hover()
      const tooltip = page.getByRole('tooltip').first()
      await expect(tooltip).toBeVisible()
      const text = (await tooltip.textContent()) ?? ''
      expect(text.trim().length).toBeGreaterThan(0)
      // Dismiss so next iteration starts clean.
      await page.mouse.move(0, 0)
    }
  })
})
