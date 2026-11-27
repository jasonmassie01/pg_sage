// session-end.spec.ts (dashboard-16): apiauth-01 CHECK-20. A user an admin
// deactivates keeps an open tab; its next poll answers 401 (ValidateSession
// refuses a disabled user's session) and the tab shows sign-in, once, with
// no redirect loop. The two-tab case is the only multi-page test here.
// Payloads: e2e/mocks/shell.ts.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { NOT_AUTHENTICATED, logAPIRequests, routeOutage } from './mocks/shell'

type Session = { deactivated: boolean }

// PUBLIC are the gatePublic auth routes: they answer without a session.
const PUBLIC = ['/api/v1/auth/mode', '/api/v1/auth/oauth/config', '/api/v1/auth/login']

// routeSession answers every non-public API request 401 once the user is
// deactivated, /auth/me included, as the session middleware does. The mode
// endpoint answers {"mode": "session"} (databricks_app_auth.go).
async function routeSession(page: Page, session: Session) {
  await mockAllAPIs(page)
  await page.route('**/api/v1/auth/mode', route => route.fulfill({ json: { mode: 'session' } }))
  await routeOutage(page, req => (session.deactivated &&
    !PUBLIC.includes(new URL(req.url()).pathname) ? NOT_AUTHENTICATED : null))
}

function isSessionCheck(path: string) {
  return path.startsWith('/api/v1/auth/me')
}

function mainFrameNavigations(page: Page) {
  let count = 0
  page.on('framenavigated', frame => { if (frame === page.mainFrame()) count += 1 })
  return () => count
}

async function signedIn(page: Page) {
  // ux-11: Actions is Changes at #/changes.
  await page.goto('/#/changes')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Changes')
  await expect(page.getByTestId('user-email')).toBeVisible()
}

async function onSignIn(page: Page) {
  await expect(page.getByTestId('login-email')).toBeVisible()
  await expect(page.getByTestId('login-submit')).toBeVisible()
  await expect(page.getByTestId('user-email')).toHaveCount(0)
  await expect(page.getByTestId('app-error')).toHaveCount(0)
}

test('apiauth-01 CHECK-20: a deactivated user\'s open tab goes to sign-in on its next poll',
  async ({ page }) => {
    const session = { deactivated: false }
    await page.clock.install()
    await routeSession(page, session)
    const log = logAPIRequests(page)
    const navigations = mainFrameNavigations(page)
    await signedIn(page)

    const bootChecks = log.filter(isSessionCheck).length
    session.deactivated = true
    // Nothing changes until a request is made: the tab is still signed in.
    await expect(page.getByTestId('user-email')).toBeVisible()
    // The fleet inventory polls every 30 s (App useAPI interval).
    await page.clock.fastForward(30_000)
    await onSignIn(page)

    // Once, with no loop: no reload, no second session check, and the
    // signed-out screen stops polling.
    const mark = log.length
    await page.clock.fastForward(120_000)
    await page.waitForTimeout(500)
    await onSignIn(page)
    expect(navigations()).toBe(1)
    expect(log.filter(isSessionCheck)).toHaveLength(bootChecks)
    const after = log.slice(mark)
    expect(after.filter(p => !p.startsWith('/api/v1/auth/oauth/config'))).toEqual([])
  })

test('a 401 from the session check itself is sign-in, not an expiry loop',
  async ({ page }) => {
    const session = { deactivated: true }
    await routeSession(page, session)
    const log = logAPIRequests(page)
    const navigations = mainFrameNavigations(page)
    await page.goto('/#/changes')
    await onSignIn(page)
    const atSignIn = log.filter(isSessionCheck).length
    await page.waitForTimeout(1_000)
    await onSignIn(page)
    expect(navigations()).toBe(1)
    // The boot check (run twice by React StrictMode under the dev server),
    // and none after it.
    expect(atSignIn).toBeGreaterThanOrEqual(1)
    expect(atSignIn).toBeLessThanOrEqual(2)
    expect(log.filter(isSessionCheck)).toHaveLength(atSignIn)
    // Signed out from the start: the inventory is never asked for.
    expect(log.filter(p => p.startsWith('/api/v1/databases'))).toEqual([])
  })

test('two tabs of one browser: deactivation sends both to sign-in', async ({ page, context }) => {
  // One session cookie, two tabs: the server refuses it for both.
  const session = { deactivated: false }
  await context.clock.install()
  await routeSession(page, session)
  const second = await context.newPage()
  await routeSession(second, session)
  const firstNavigations = mainFrameNavigations(page)
  const secondNavigations = mainFrameNavigations(second)
  await signedIn(page)
  await signedIn(second)

  session.deactivated = true
  await context.clock.fastForward(30_000)
  await onSignIn(page)
  await onSignIn(second)
  expect(firstNavigations()).toBe(1)
  expect(secondNavigations()).toBe(1)
})
