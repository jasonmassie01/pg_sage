// dashboard.spec.ts — Smoke tests for the Dashboard page.
//
// Verifies that the health hero, stat cards, database list,
// and recent findings section all render with mock data.

import { test, expect } from '@playwright/test'
import { mockAllAPIs, mockDatabases } from './fixtures'

test.describe('Dashboard page', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
  })

  // cases-11: one warning finding is open work, so the hero makes no
  // health claim; only a summary with every count at zero is clear.
  test('health hero reports the open warning instead of health',
    async ({ page }) => {
      await page.goto('/#/advanced')

      const hero = page.locator('[data-testid="health-hero"]')
      await expect(hero).toBeVisible()
      await expect(hero).toContainText('1 warning finding')
      await expect(hero).not.toContainText('No open critical or warning issues')
      await expect(page.getByTestId('health-hero-cases-link'))
        .toHaveAttribute('href', '#/cases')
    })

  test('health hero is clear only when every count is zero',
    async ({ page }) => {
      await page.route('**/api/v1/databases', route => {
        if (route.request().url().includes('/managed')) {
          return route.continue()
        }
        return route.fulfill({
          json: {
            ...mockDatabases,
            summary: { ...mockDatabases.summary, total_warning: 0 },
          },
        })
      })
      await page.goto('/#/advanced')

      const hero = page.locator('[data-testid="health-hero"]')
      await expect(hero).toContainText('No open critical or warning issues')
    })

  test('stat cards display correct counts', async ({ page }) => {
    await page.goto('/#/advanced')

    const dbStat = page.locator('[data-testid="stat-databases"]')
    await expect(dbStat).toBeVisible()
    await expect(dbStat).toContainText('Databases')
    await expect(dbStat).toContainText(
      String(mockDatabases.summary.total_databases),
    )

    const healthyStat = page.locator('[data-testid="stat-healthy"]')
    await expect(healthyStat).toBeVisible()
    await expect(healthyStat).toContainText('Healthy')
    await expect(healthyStat).toContainText(
      String(mockDatabases.summary.healthy),
    )
  })

  test('database list table renders with entries', async ({ page }) => {
    await page.goto('/#/advanced')

    const list = page.locator('[data-testid="db-list"]')
    await expect(list).toBeVisible()
    await expect(list).toContainText('Databases')

    // Each database from mock data should appear as a list item.
    const items = page.locator('[data-testid="db-list-item"]')
    await expect(items).toHaveCount(mockDatabases.databases.length)

    // First database name should be visible.
    await expect(items.first()).toContainText('primary')
  })

  test('database list items show health scores', async ({ page }) => {
    await page.goto('/#/advanced')

    const items = page.locator('[data-testid="db-list-item"]')
    await expect(items.first()).toContainText('Health')
    await expect(items.first()).toContainText('95')
    await expect(items.nth(1)).toContainText('Health')
    await expect(items.nth(1)).toContainText('88')
  })

  test('trust level badges render on database items', async ({ page }) => {
    await page.goto('/#/advanced')

    const advisoryBadge = page.locator('[data-testid="trust-badge-advisory"]')
    await expect(advisoryBadge).toBeVisible()
    await expect(advisoryBadge).toHaveAttribute(
      'title',
      'SAFE actions may auto-execute only when execution_mode=auto, tier3_safe is on and the 8-day trust ramp has passed; otherwise they queue for approval',
    )
    await expect(page.locator('[data-testid="trust-badge-observation"]'))
      .toBeVisible()
  })

  test('top cases section appears with data', async ({ page }) => {
    await page.goto('/#/advanced')
    await page.locator('[data-testid="overview-tab-top-cases"]').click()

    const topCases = page.locator('[data-testid="top-cases"]')
    await expect(topCases).toBeVisible()
    await expect(topCases).toContainText('Top issues')
    await expect(topCases).toContainText('Stats are stale')
    await expect(topCases.locator('[data-testid="top-case-link"]').first())
      .toHaveAttribute('href', /^#\/cases\?case=/)
  })

  test('health hero shows issues when data has degraded dbs',
    async ({ page }) => {
      // Override the databases route to return degraded data.
      await page.route('**/api/v1/databases', route => {
        if (route.request().url().includes('/managed')) {
          return route.continue()
        }
        return route.fulfill({
          json: {
            summary: {
              total_databases: 2,
              healthy: 1,
              degraded: 1,
              total_critical: 1,
              emergency_stopped: false,
            },
            databases: mockDatabases.databases,
          },
        })
      })

      await page.goto('/#/advanced')

      const hero = page.locator('[data-testid="health-hero"]')
      await expect(hero).toBeVisible()
      await expect(hero).toContainText('Need Attention')
    })

  test('onboarding welcome shows when no databases exist',
    async ({ page }) => {
      // Override databases to return empty.
      await page.route('**/api/v1/databases', route => {
        if (route.request().url().includes('/managed')) {
          return route.continue()
        }
        return route.fulfill({
          json: {
            summary: {
              total_databases: 0,
              healthy: 0,
              degraded: 0,
              total_critical: 0,
              emergency_stopped: false,
            },
            databases: [],
          },
        })
      })

      await page.goto('/#/advanced')

      const welcome = page.locator('[data-testid="onboarding-welcome"]')
      await expect(welcome).toBeVisible()
      await expect(welcome).toContainText('Welcome to pg_sage')
      await expect(welcome).toContainText('Add your first database')
    })
})
