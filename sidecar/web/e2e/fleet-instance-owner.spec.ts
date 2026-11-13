// fleet-instance-owner.spec.ts — advisors-08b: the fleet overview and the
// provider readiness matrix name which database owns each PostgreSQL
// instance's settings (status.capabilities.instance_owner, advisors-08).
//
// The instance_owner objects copy the JSON shape pinned by
// TestEnsureCapabilities_ReportsInstanceOwner (internal/fleet).

import { test, expect, type Page } from '@playwright/test'
import { mockAllAPIs, mockDatabases } from './fixtures'

const ownerOf = (isOwner: boolean) => ({
  owner: 'primary',
  is_owner: isOwner,
  key_source: 'system_identifier',
})

const capabilities = (isOwner: boolean) => ({
  provider: 'self-managed',
  ready_for_auto_safe: true,
  instance_owner: ownerOf(isOwner),
})

const status = (healthScore: number, isOwner: boolean) => ({
  connected: true,
  error: null,
  health_score: healthScore,
  findings_critical: 0,
  findings_warning: 0,
  platform: 'self-managed',
  capabilities: capabilities(isOwner),
})

const fleet = {
  ...mockDatabases,
  summary: { ...mockDatabases.summary, total_warning: 0 },
  databases: [
    { name: 'primary', trust_level: 'advisory', status: status(95, true) },
    { name: 'reporting', trust_level: 'advisory', status: status(90, false) },
  ],
}

const readiness = {
  mode: 'fleet',
  summary: { total_databases: 2, ready_for_auto_safe: 2, blocked: 0 },
  databases: fleet.databases.map(db => ({
    name: db.name,
    provider: 'self-managed',
    ready_for_auto_safe: true,
    capabilities: db.status.capabilities,
  })),
}

async function mockFleet(page: Page) {
  await mockAllAPIs(page)
  // Registered after mockAllAPIs, so these win (Playwright matches LIFO).
  await page.route('**/api/v1/databases**', route => {
    const path = new URL(route.request().url()).pathname
    if (path !== '/api/v1/databases') return route.fallback()
    return route.fulfill({ json: fleet })
  })
  await page.route('**/api/v1/fleet/readiness**', route =>
    route.fulfill({ json: readiness }),
  )
}

test.describe('Fleet instance owner', () => {
  test.beforeEach(async ({ page }) => {
    await mockFleet(page)
  })

  test('database tiles say which database owns the instance settings',
    async ({ page }) => {
      await page.goto('/#/advanced')

      const tiles = page.locator('[data-testid="db-list-item"]')
      await expect(tiles).toHaveCount(2)
      await expect(tiles.nth(0).locator('[data-testid="db-instance-owner"]'))
        .toHaveText('Owns the instance settings')
      await expect(tiles.nth(1).locator('[data-testid="db-instance-owner"]'))
        .toHaveText('Instance settings handled by primary')
    })

  test('readiness matrix shows the Instance settings column',
    async ({ page }) => {
      await page.goto('/#/advanced')
      await page.locator('[data-testid="overview-tab-provider-readiness"]').click()

      const matrix = page.locator('[data-testid="provider-readiness-matrix"]')
      await expect(matrix).toBeVisible()
      await expect(matrix.locator('th', { hasText: 'Instance settings' }))
        .toBeVisible()
      const cells = matrix.locator('[data-testid="readiness-instance-owner"]')
      await expect(cells).toHaveText([
        'Owns the instance settings',
        'Instance settings handled by primary',
      ])
    })
})
