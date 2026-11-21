// shell-resilience.spec.ts (dashboard-16): the app shell through a proxy
// 502 (dashboard-01 CHECK-18: the refresh strip, never the page error
// boundary), a sidecar restart (dashboard-04 CHECK-12: Live again without a
// reload) and a session-store outage (apiauth-02 CHECK-14: 503s and a banner,
// never sign-out). Payloads: e2e/mocks/shell.ts.
// No concurrency tests: each test drives one tab; session-end.spec.ts has the
// two-tab case.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  STORE_DOWN, advanceUntil, logAPIRequests, proxy502, routeEvents, routeOutage,
} from './mocks/shell'

const STRIP_502 = new RegExp('^Refresh failed at \\d{2}:\\d{2} \\(Request failed ' +
  '\\(502 Bad Gateway\\)\\)\\. Showing data from \\d{2}:\\d{2}')

// watchPageErrors collects uncaught page exceptions: a crash on the HTML
// body would land here even if no boundary drew it.
function watchPageErrors(page: Page) {
  const errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  return errors
}

// mainFrameNavigations counts main-frame navigations: the goto is one, a
// reload or a redirect would add more.
function mainFrameNavigations(page: Page) {
  let count = 0
  page.on('framenavigated', frame => { if (frame === page.mainFrame()) count += 1 })
  return () => count
}

async function noErrorBoundary(page: Page) {
  await expect(page.getByTestId('page-error')).toHaveCount(0)
  await expect(page.getByTestId('app-error')).toHaveCount(0)
}

function isFleet(url: string) {
  return new URL(url).pathname === '/api/v1/databases'
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('dashboard-01 CHECK-18: a proxy 502 shows the strip and keeps the snapshot',
  async ({ page }) => {
    const errors = watchPageErrors(page)
    let fleetDown = false
    let served502 = 0
    await page.clock.install()
    await routeOutage(page, req => {
      if (!fleetDown || !isFleet(req.url())) return null
      served502 += 1
      return proxy502
    })
    await page.goto('/#/manage-databases?tab=readiness')
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    await expect(page.getByTestId('query-stale')).toHaveCount(0)

    fleetDown = true
    // The fleet inventory polls every 30 s (App useAPI interval).
    await page.clock.fastForward(30_000)
    const strip = page.getByTestId('query-stale')
    await expect(strip).toHaveText(STRIP_502)
    expect(served502).toBe(1)
    // The data from before the 502 stays on screen under the strip.
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    await expect(page.getByTestId('stat-healthy')).toContainText('2')
    await noErrorBoundary(page)

    fleetDown = false
    await strip.getByRole('button', { name: 'Retry' }).click()
    await expect(strip).toHaveCount(0)
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    expect(errors).toEqual([])
  })

test('a 502 with an HTML body on the first load shows the banner with Retry',
  async ({ page }) => {
    const errors = watchPageErrors(page)
    let fleetDown = true
    await routeOutage(page, req => (fleetDown && isFleet(req.url()) ? proxy502 : null))
    await page.goto('/#/manage-databases?tab=readiness')
    const banner = page.getByRole('alert').filter({ hasText: 'Request failed (502 Bad Gateway)' })
    await expect(banner.first()).toBeVisible()
    // Nothing from the failed inventory is drawn as if it were data.
    await expect(page.getByTestId('stat-databases')).toHaveCount(0)
    await noErrorBoundary(page)

    fleetDown = false
    await banner.first().getByRole('button', { name: 'Retry' }).click()
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    await expect(banner).toHaveCount(0)
    expect(errors).toEqual([])
  })

test('dashboard-04 CHECK-12: Live, not live during a restart, Live again without a reload',
  async ({ page }) => {
    const errors = watchPageErrors(page)
    const navigations = mainFrameNavigations(page)
    let down = false
    let served502 = 0
    await page.clock.install()
    await routeEvents(page)
    // A restart 502s every API path, the event stream included.
    await routeOutage(page, () => {
      if (!down) return null
      served502 += 1
      return proxy502
    })
    await page.goto('/#/manage-databases?tab=readiness')
    const freshness = page.getByTestId('freshness-indicator')
    await expect(freshness).toHaveAttribute('data-status', 'live')
    await expect(freshness).toHaveText('Live')
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    await page.evaluate(() => Object.assign(window, { __sageSameDocument: true }))

    down = true
    const strip = page.getByTestId('query-stale')
    await advanceUntil(page, async () =>
      (await freshness.getAttribute('data-status')) !== 'live' && await strip.isVisible())
    await expect(freshness).toHaveAttribute('data-status', /^(reconnecting|offline)$/)
    await expect(freshness).not.toHaveText('Live')
    await expect(strip).toHaveText(STRIP_502)
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    expect(served502).toBeGreaterThan(1)

    down = false
    await advanceUntil(page, async () =>
      (await freshness.getAttribute('data-status')) === 'live' && (await strip.count()) === 0)
    await expect(freshness).toHaveText('Live')
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    // Same document, one navigation: the recovery needed no reload.
    expect(await page.evaluate(() =>
      (window as unknown as { __sageSameDocument?: boolean }).__sageSameDocument)).toBe(true)
    expect(navigations()).toBe(1)
    await noErrorBoundary(page)
    expect(errors).toEqual([])
  })

test('apiauth-02 CHECK-14: a control-database outage shows the banner, never sign-out',
  async ({ page }) => {
    let storeDown = false
    let served503 = 0
    await page.clock.install()
    // Session auth sits in front of every signed-in route, so every API
    // request answers 503 while the store is down.
    await routeOutage(page, () => {
      if (!storeDown) return null
      served503 += 1
      return STORE_DOWN
    })
    const log = logAPIRequests(page)
    await page.goto('/#/manage-databases?tab=readiness')
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    await expect(page.getByTestId('auth-store-banner')).toHaveCount(0)
    const bootChecks = log.filter(p => p.startsWith('/api/v1/auth/me')).length

    storeDown = true
    await page.clock.fastForward(30_000)
    const banner = page.getByTestId('auth-store-banner')
    await expect(banner).toHaveText('Session store unavailable, retrying. You are still signed in.')
    // Two more fleet polls (the second after backoff) still answer 503.
    await page.clock.fastForward(30_000)
    await page.clock.fastForward(60_000)
    await expect(banner).toBeVisible()
    await expect(page.getByTestId('user-email')).toBeVisible()
    await expect(page.getByTestId('login-email')).toHaveCount(0)
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    expect(served503).toBeGreaterThanOrEqual(3)

    storeDown = false
    await advanceUntil(page, async () => (await banner.count()) === 0)
    await expect(page.getByTestId('user-email')).toBeVisible()
    await expect(page.getByTestId('login-email')).toHaveCount(0)
    // The outage never logged out or re-checked the session.
    expect(log.filter(p => p.startsWith('/api/v1/auth/logout'))).toEqual([])
    expect(log.filter(p => p.startsWith('/api/v1/auth/me'))).toHaveLength(bootChecks)
  })

test('a 503 without the store code is some other outage: strip, no session banner',
  async ({ page }) => {
    let down = false
    await page.clock.install()
    await routeOutage(page, req => (down && isFleet(req.url())
      ? { status: 503, json: { error: 'fleet manager unavailable' } } : null))
    await page.goto('/#/manage-databases?tab=readiness')
    await expect(page.getByTestId('stat-databases')).toContainText('2')

    down = true
    await page.clock.fastForward(30_000)
    await expect(page.getByTestId('query-stale')).toContainText('(fleet manager unavailable (503))')
    await expect(page.getByTestId('stat-databases')).toContainText('2')
    await expect(page.getByTestId('auth-store-banner')).toHaveCount(0)
    await expect(page.getByTestId('user-email')).toBeVisible()
  })
