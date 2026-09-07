// token-budget.spec.ts — Tests for the TokenBudgetBanner component.
//
// Verifies that the banner appears when LLM token budget is exhausted,
// hides when not exhausted, and the reset button works.

import { test, expect } from '@playwright/test'
import { mockAllAPIs, mockLLMStatusExhausted } from './fixtures'

test.describe('Token budget banner', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
  })

  test('banner is hidden when budget is not exhausted', async ({
    page,
  }) => {
    await page.goto('/#/advanced')

    // Default mock has budget_exhausted: false — banner should not
    // appear.
    await expect(
      page.locator('[data-testid="token-budget-banner"]'),
    ).not.toBeVisible()
  })

  test('banner shows on dashboard when budget is exhausted', async ({
    page,
  }) => {
    // Override LLM status to return exhausted budget.
    await page.route('**/api/v1/llm/status', route =>
      route.fulfill({ json: mockLLMStatusExhausted }),
    )

    await page.goto('/#/advanced')

    const banner = page.locator('[data-testid="token-budget-banner"]')
    await expect(banner).toBeVisible()
    await expect(banner).toContainText('LLM token budget exhausted')
    await expect(banner).toContainText('General')
    await expect(banner).toContainText('Optimizer')
    await expect(banner).toContainText('500K / 500K')
    await expect(banner).toContainText('200K / 200K')
  })

  test('banner shows only the exhausted client', async ({ page }) => {
    // Only general is exhausted, optimizer is not.
    await page.route('**/api/v1/llm/status', route =>
      route.fulfill({
        json: {
          general: {
            ...mockLLMStatusExhausted.general,
            budget_exhausted: true,
          },
          optimizer: {
            ...mockLLMStatusExhausted.optimizer,
            budget_exhausted: false,
          },
        },
      }),
    )

    await page.goto('/#/advanced')

    const banner = page.locator('[data-testid="token-budget-banner"]')
    await expect(banner).toBeVisible()
    await expect(banner).toContainText('General')
    await expect(banner).not.toContainText('Optimizer')
  })

  test('reset button calls budget reset endpoint', async ({ page }) => {
    let resetCalled = false

    await page.route('**/api/v1/llm/status', route =>
      route.fulfill({ json: mockLLMStatusExhausted }),
    )
    await page.route('**/api/v1/llm/budget/reset', route => {
      resetCalled = true
      return route.fulfill({ status: 200, json: { ok: true } })
    })

    await page.goto('/#/advanced')

    const resetBtn = page.locator('[data-testid="token-budget-reset"]')
    await expect(resetBtn).toBeVisible()
    await expect(resetBtn).toContainText('Reset Budget')

    await resetBtn.click()

    // Verify the endpoint was called.
    expect(resetCalled).toBe(true)
  })

  test('banner shows on settings LLM tab when exhausted', async ({
    page,
  }) => {
    await page.route('**/api/v1/llm/status', route =>
      route.fulfill({ json: mockLLMStatusExhausted }),
    )

    await page.goto('#/settings')

    // Switch to advanced mode to see the LLM tab.
    await page.locator(
      '[data-testid="settings-mode-toggle"]',
    ).click()
    await page.locator('[data-testid="settings-tab-llm"]').click()

    const banner = page.locator('[data-testid="token-budget-banner"]')
    await expect(banner.first()).toBeVisible()
    await expect(banner.first()).toContainText(
      'LLM token budget exhausted',
    )
  })

  test('banner shows on settings AI & Alerts tab when exhausted',
    async ({ page }) => {
      await page.route('**/api/v1/llm/status', route =>
        route.fulfill({ json: mockLLMStatusExhausted }),
      )

      // Clear localStorage so we start in simple mode.
      await page.addInitScript(() => {
        localStorage.removeItem('pg_sage_settings_mode')
      })

      await page.goto('#/settings')

      // Switch to AI & Alerts tab (simple mode).
      await page.locator(
        '[data-testid="settings-tab-ai-alerts"]',
      ).click()

      const banner = page.locator(
        '[data-testid="token-budget-banner"]',
      )
      await expect(banner.first()).toBeVisible()
      await expect(banner.first()).toContainText(
        'LLM token budget exhausted',
      )
    })
})
