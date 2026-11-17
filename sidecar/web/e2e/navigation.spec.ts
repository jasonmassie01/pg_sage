// navigation.spec.ts — Smoke tests for sidebar navigation.
//
// Verifies that all nav links render, are clickable, and
// update the URL hash correctly. Uses route mocking so no
// live backend is required.

import { test, expect } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

test.describe('Sidebar navigation', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
  })

  test('sidebar renders all nav group headings', async ({ page }) => {
    await page.goto('/')
    await expect(page.locator('nav')).toBeVisible()

    // Layout now contracts around autonomous DBA operation.
    for (const heading of ['Operate']) {
      await expect(
        page.locator('nav').getByText(heading, { exact: true }),
      ).toBeVisible()
    }
  })

  test('all non-admin nav links are visible', async ({ page }) => {
    await page.goto('/')

    // These nav items are always visible (no admin gate).
    const navItems = [
      { testid: 'nav-inbox', label: 'Inbox' },
      { testid: 'nav-cases', label: 'Cases' },
      { testid: 'nav-actions', label: 'Actions' },
    ]

    for (const item of navItems) {
      const link = page.locator(`[data-testid="${item.testid}"]`)
      await expect(link).toBeVisible()
      await expect(link).toContainText(item.label)
    }
  })

  test('admin nav links are visible for admin user', async ({ page }) => {
    await page.goto('/')

    // Our mock user has role=admin, so admin-only items should appear.
    const adminItems = [
      { testid: 'nav-databases', label: 'Fleet' },
      { testid: 'nav-settings', label: 'Settings' },
    ]

    for (const item of adminItems) {
      const link = page.locator(`[data-testid="${item.testid}"]`)
      await expect(link).toBeVisible()
      await expect(link).toContainText(item.label)
    }
  })

  test('clicking nav links updates the URL hash', async ({ page }) => {
    await page.goto('/')

    const routes = [
      { testid: 'nav-cases', hash: '#/cases' },
      { testid: 'nav-actions', hash: '#/actions' },
      { testid: 'nav-settings', hash: '#/settings' },
      { testid: 'nav-inbox', hash: '#/' },
    ]

    for (const route of routes) {
      await page.locator(`[data-testid="${route.testid}"]`).click()
      await expect(page).toHaveURL(new RegExp(`${route.hash}$`))
    }
  })

  test('page header shows current section name', async ({ page }) => {
    await page.goto('/')

    const header = page.locator('header h1')
    await expect(header).toContainText('Inbox')

    await page.locator('[data-testid="nav-settings"]').click()
    await expect(header).toContainText('Settings')

    await page.locator('[data-testid="nav-cases"]').click()
    await expect(header).toContainText('Cases')
  })

  test('user email and sign-out button render', async ({ page }) => {
    await page.goto('/')

    await expect(
      page.locator('[data-testid="user-email"]'),
    ).toContainText('test@test.com')

    await expect(
      page.locator('[data-testid="sign-out-button"]'),
    ).toBeVisible()
  })

  test('no console errors during navigation', async ({ page }) => {
    const errors: string[] = []
    page.on('console', msg => {
      if (msg.type() === 'error') {
        errors.push(msg.text())
      }
    })

    await page.goto('/')
    await page.locator('[data-testid="nav-cases"]').click()
    await page.locator('[data-testid="nav-settings"]').click()
    await page.locator('[data-testid="nav-inbox"]').click()

    // Filter out known benign errors (e.g. favicon 404).
    const real = errors.filter(
      e => !e.includes('favicon') && !e.includes('404')
        && !e.includes('EventSource'),
    )
    expect(real).toHaveLength(0)
  })
})

// dashboard-03: a page whose payload has the wrong shape throws during
// render. The page boundary keeps the header emergency stop and the
// navigation mounted, and the next route renders normally.
test.describe('Page render error', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
    // Registered last, so it wins over the catch-all: a string where the
    // SLOs page expects a list.
    await page.route('**/api/v1/sre/slos**', route =>
      route.fulfill({ json: { slos: 'not-a-list' } }),
    )
  })

  test('emergency stop stays visible and clickable', async ({ page }) => {
    await page.goto('/#/slos')

    await expect(page.getByTestId('page-error'))
      .toContainText('This page failed to render')
    await expect(page.getByTestId('sidebar-desktop')).toBeVisible()
    const stop = page.getByTestId('header-emergency-stop')
    await expect(stop).toBeVisible()
    await stop.click()
    await expect(page.getByTestId('header-emergency-stop-dialog')).toBeVisible()
  })

  test('navigating away renders the next page', async ({ page }) => {
    await page.goto('/#/slos')
    await expect(page.getByTestId('page-error')).toBeVisible()

    await page.getByTestId('nav-actions').click()

    await expect(page).toHaveURL(/#\/actions$/)
    await expect(page.getByTestId('page-error')).toHaveCount(0)
    await expect(page.locator('header h1')).toContainText('Actions')
  })
})
