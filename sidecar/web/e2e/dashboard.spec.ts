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

  test('health hero renders with healthy status', async ({ page }) => {
    await page.goto('/')

    const hero = page.locator('[data-testid="health-hero"]')
    await expect(hero).toBeVisible()
    // Mock data has 0 degraded + 0 critical => "All Systems Healthy"
    await expect(hero).toContainText('All Systems Healthy')
  })

  test('stat cards display correct counts', async ({ page }) => {
    await page.goto('/')

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
    await page.goto('/')

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
    await page.goto('/')

    const items = page.locator('[data-testid="db-list-item"]')
    await expect(items.first()).toContainText('Health')
    await expect(items.first()).toContainText('95')
    await expect(items.nth(1)).toContainText('Health')
    await expect(items.nth(1)).toContainText('88')
  })

  test('trust level badges render on database items', async ({ page }) => {
    await page.goto('/')

    const advisoryBadge = page.locator('[data-testid="trust-badge-advisory"]')
    await expect(advisoryBadge).toBeVisible()
    await expect(advisoryBadge).toHaveAttribute(
      'title',
      'SAFE actions executed autonomously',
    )
    await expect(page.locator('[data-testid="trust-badge-observation"]'))
      .toBeVisible()
  })

  test('recent findings section appears with data', async ({ page }) => {
    await page.goto('/')

    const findings = page.locator('[data-testid="recent-findings"]')
    await expect(findings).toBeVisible()
    await expect(findings).toContainText('Recent Recommendations')
    await expect(findings).toContainText('Unused index')
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

      await page.goto('/')

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

      await page.goto('/')

      const welcome = page.locator('[data-testid="onboarding-welcome"]')
      await expect(welcome).toBeVisible()
      await expect(welcome).toContainText('Welcome to pg_sage')
      await expect(welcome).toContainText('Add your first database')
    })
})
