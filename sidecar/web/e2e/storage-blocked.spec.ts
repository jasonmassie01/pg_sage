// storage-blocked.spec.ts (dashboard-16): dashboard-03 CHECK-06, a browser
// with site data blocked. The localStorage and sessionStorage accessors
// throw SecurityError (e2e/mocks/shell.ts blockStorage), as Chrome's "Block
// sites from saving data" does; preferences fall back and nothing crashes.
// No concurrency tests: storage is per tab and each test drives one tab.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { blockStorage } from './mocks/shell'

function watchPageErrors(page: Page) {
  const errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  return errors
}

// accessorError is the DOMException name reading window[name] throws, or
// 'none' when the accessor answers.
function accessorError(page: Page, name: 'localStorage' | 'sessionStorage') {
  return page.evaluate(n => {
    try {
      void (window as unknown as Record<string, unknown>)[n]
      return 'none'
    } catch (err) {
      return err instanceof DOMException ? err.name : `not a DOMException: ${err}`
    }
  }, name)
}

async function noErrorBoundary(page: Page) {
  await expect(page.getByTestId('page-error')).toHaveCount(0)
  await expect(page.getByTestId('app-error')).toHaveCount(0)
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('dashboard-03 CHECK-06: with site data blocked the dashboard loads and works',
  async ({ page }) => {
    const errors = watchPageErrors(page)
    await blockStorage(page)
    await page.goto('/#/advanced')
    // The double is in force: both accessors throw, as with site data blocked.
    expect(await accessorError(page, 'localStorage')).toBe('SecurityError')
    expect(await accessorError(page, 'sessionStorage')).toBe('SecurityError')

    await expect(page.getByTestId('stat-databases')).toContainText('2')
    // The stored scope cannot be read: the scope falls back to all databases.
    const picker = page.getByLabel('Database scope')
    await expect(picker).toHaveValue('all')
    // Choosing a database cannot be saved, and still changes the scope.
    await picker.selectOption('primary')
    await expect(picker).toHaveValue('primary')
    await expect(page.getByTestId('monitoring-context')).toContainText('primary')

    // Pages with their own stored preferences render too.
    // Home is the Inbox since ux-01b; ux-11 renamed Actions to Changes and
    // folded Value into the ledger.
    for (const [hash, title] of [['#/settings', 'Settings'], ['#/actions', 'Changes'],
      ['#/', 'Inbox'], ['#/value', 'Outcome ledger']]) {
      await page.evaluate(h => { window.location.hash = h }, hash)
      await expect(page.locator('header h1')).toHaveText(title)
      await noErrorBoundary(page)
    }
    expect(errors).toEqual([])
  })

// ux-05c: a visit cannot be remembered with site data blocked, so the page
// guide stays a collapsed "About this page" toggle instead of opening on
// every page; it still opens on a click. Phones hide it either way.
for (const width of [1280, 375]) {
  test(`with site data blocked the page guide is collapsed at ${width}px`, async ({ page }) => {
    const errors = watchPageErrors(page)
    await page.setViewportSize({ width, height: 900 })
    await blockStorage(page)
    for (const hash of ['#/', '#/cases', '#/settings']) {
      await page.goto('/' + hash)
      await expect(page.locator('header h1')).not.toBeEmpty()
      await expect(page.getByTestId('postgres-meme')).toHaveCount(0)
      const toggle = page.getByRole('button', { name: 'About this page' })
      if (width < 768) {
        await expect(toggle).toHaveCount(0)
        continue
      }
      await expect(toggle).toHaveAttribute('aria-expanded', 'false')
      await toggle.click()
      await expect(page.getByTestId('tab-introduction')).toBeVisible()
      await noErrorBoundary(page)
    }
    expect(errors).toEqual([])
  })
}

test('a blocked store forgets the scope on reload; a working one keeps it',
  async ({ page, context }) => {
    // State transition: with storage, the chosen scope survives a reload.
    await page.goto('/#/advanced')
    const picker = page.getByLabel('Database scope')
    await picker.selectOption('primary')
    await page.reload()
    await expect(page.getByLabel('Database scope')).toHaveValue('primary')

    // Blocked: the same choice falls back to all on reload, without an error.
    const blocked = await context.newPage()
    const errors = watchPageErrors(blocked)
    await mockAllAPIs(blocked)
    await blockStorage(blocked)
    await blocked.goto('/#/advanced')
    const blockedPicker = blocked.getByLabel('Database scope')
    await expect(blockedPicker).toHaveValue('all')
    await blockedPicker.selectOption('replica')
    await expect(blockedPicker).toHaveValue('replica')
    await blocked.reload()
    await expect(blocked.getByLabel('Database scope')).toHaveValue('all')
    await expect(blocked.getByTestId('stat-databases')).toContainText('2')
    await noErrorBoundary(blocked)
    expect(errors).toEqual([])
  })
