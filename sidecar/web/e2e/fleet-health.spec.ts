// fleet-health.spec.ts (dashboard-18): the health-score tooltip and fleet
// order on the database tiles (fleet-07, reports/t1/fleet-07.md:128 MANUAL),
// and a two-database fleet with one database refusing connections on the
// Value page (firstlook-03 CHECK-05, reports/t1/firstlook-03.md:170, UI half;
// a real stopped Postgres is dashboard-17 CHECK-18). Payloads:
// e2e/mocks/fleet.ts. No concurrency tests: each test drives one page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { routeFleet } from './mocks/cases'
import { LOG_RCA_DETAIL } from './mocks/firstlook'
import {
  CONNECTED_DOWN, WORST_FIRST, firstLookFleet, inventory, onboardingEntry, routeInventory,
  routeSetup, scored,
} from './mocks/fleet'

const tiles = (page: Page) => page.getByTestId('db-tile-grid').getByTestId('db-list-item')
const tile = (page: Page, name: string) => tiles(page).filter({ hasText: name })
const score = (page: Page, name: string) => tile(page, name).locator('div.text-2xl')

test.describe('fleet-07: health scores on the database tiles', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
  })

  test('fleet-07: each score carries its reasons as a tooltip and a list',
    async ({ page }) => {
      await routeInventory(page, () => inventory([
        ...WORST_FIRST.slice(0, 2),
        scored('stale', 25, ['collector stale 42m', '2 warnings'], { findings_warning: 2 }),
        WORST_FIRST[4],
      ]))
      await page.goto('/#/advanced')
      await expect(score(page, 'mike')).toHaveText('0')
      await expect(score(page, 'mike')).toHaveAttribute('title',
        'disconnected: connection refused')
      await expect(score(page, 'yankee')).toHaveAttribute('title', '1 critical finding')
      await expect(score(page, 'stale')).toHaveText('25')
      await expect(score(page, 'stale')).toHaveAttribute('title',
        'collector stale 42m; 2 warnings')
      await expect(tile(page, 'stale').getByTestId('health-reasons').locator('li'))
        .toHaveText(['collector stale 42m', '2 warnings'])
      // A disconnected database is never "Clean", whatever its findings.
      await expect(tile(page, 'mike')).not.toContainText('Clean')
      // A full score has no reasons: no tooltip, no list.
      await expect(score(page, 'zeta')).toHaveText('100')
      await expect(score(page, 'zeta')).not.toHaveAttribute('title', /.*/)
      await expect(tile(page, 'zeta').getByTestId('health-reasons')).toHaveCount(0)
      await expect(tile(page, 'zeta')).toContainText('Clean')
    })

  test('fleet-07: equal scores are ordered by name on every reload', async ({ page }) => {
    await routeInventory(page, () => inventory(WORST_FIRST))
    const names = WORST_FIRST.map(d => d.name)
    await page.goto('/#/advanced')
    for (let load = 0; load < 3; load++) {
      if (load > 0) await page.reload()
      await expect(tiles(page)).toHaveCount(names.length)
      for (const [i, name] of names.entries()) {
        await expect(tiles(page).nth(i).locator('span.font-medium')).toHaveText(name)
      }
    }
  })

  test('a score without reasons from an older server shows no tooltip', async ({ page }) => {
    const old = scored('legacy', 90, [])
    delete (old.status as Record<string, unknown>).health_reasons
    await routeInventory(page, () => inventory([old]))
    await page.goto('/#/advanced')
    await expect(score(page, 'legacy')).toHaveText('90')
    await expect(score(page, 'legacy')).not.toHaveAttribute('title', /.*/)
    await expect(tile(page, 'legacy').getByTestId('health-reasons')).toHaveCount(0)
  })

  test('an empty fleet shows the empty state, not an empty grid', async ({ page }) => {
    await routeInventory(page, () => inventory([]))
    await page.goto('/#/advanced')
    // ux-04b: the welcome moved to the setup wizard on the home page.
    await expect(page.getByTestId('snapshot-no-databases')).toBeVisible()
    await expect(page.getByTestId('db-tile-grid')).toHaveCount(0)
  })
})

test.describe('firstlook-03 CHECK-05: one database refuses connections', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
    await routeFleet(page, () => false)
  })

  const unreachable = 'pg_sage cannot reach the database reporting; check the connection ' +
    'URL and network.'

  test('firstlook-03 CHECK-05: the down database is named, the other still shows',
    async ({ page }) => {
      await routeSetup(page, { onboarding: { databases: [onboardingEntry('orders', true),
        onboardingEntry('reporting', false)] }, firstLook: firstLookFleet },
      () => ({ status: 503, json: { error: 'unavailable' } }))
      await page.goto('/#/')
      const setup = page.getByRole('region', { name: 'Getting started' })
      await expect(setup).toContainText(unreachable)
      await expect(setup).toContainText(CONNECTED_DOWN)
      await expect(setup).toContainText('orders · new install · trust observation · ' +
        'First finding 41.2 s after start')
      // The unreachable database is never counted as set up.
      await expect(setup).not.toContainText('Setup complete.')

      const firstLook = page.getByRole('region', { name: 'First look' })
      await expect(firstLook).toContainText(unreachable)
      await expect(firstLook).not.toContainText('First look running for reporting')
      await expect(firstLook.getByRole('article')
        .filter({ hasText: "Log-based RCA cannot read this server's log" }))
        .toContainText(LOG_RCA_DETAIL)
    })

  test('selecting the down database: its 503 is an alert with Retry, not a blank page',
    async ({ page }) => {
      let down = true
      await routeSetup(page, { onboarding: { databases: [] }, firstLook: firstLookFleet },
        (path, database) => {
          if (database === 'reporting' && down) {
            return { status: 503, json: { error: 'unavailable' } }
          }
          return path === '/api/v1/onboarding'
            ? { json: { databases: [onboardingEntry(database, true)] } }
            : { json: { databases: [firstLookFleet.databases[0]] } }
        })
      await page.goto('/#/')
      await page.getByRole('combobox', { name: 'Database scope' }).selectOption('reporting')
      const alerts = page.getByRole('alert')
      await expect(alerts.filter({ hasText: 'Setup status unavailable' }))
        .toContainText('Setup status unavailable: unavailable (503)')
      const firstLook = page.getByRole('region', { name: 'First look' })
      await expect(firstLook.getByRole('alert'))
        .toContainText('First look unavailable: unavailable (503)')

      down = false
      await firstLook.getByRole('alert').getByRole('button', { name: 'Retry' }).click()
      await expect(firstLook).toContainText("Log-based RCA cannot read this server's log")
    })

  test('an empty fleet answer draws neither panel', async ({ page }) => {
    await routeSetup(page, { onboarding: { databases: [] }, firstLook: { databases: [] } },
      () => ({ json: { databases: [] } }))
    await page.goto('/#/')
    await expect(page.getByRole('heading', { name: 'First look' })).toHaveCount(0)
    await expect(page.getByRole('heading', { name: 'Getting started' })).toHaveCount(0)
  })
})
