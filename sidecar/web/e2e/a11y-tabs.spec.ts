// a11y-tabs.spec.ts (ux-05d, win 17): the five tab bars share one Tabs
// component, so each is a named tablist of tabs only, the selected tab
// owns a tabpanel, and Arrow, Home and End move selection and focus.
// Payloads: the default mocks in e2e/fixtures.ts (admin user).
// No concurrency tests: each test drives one tab bar on one page.

import { expect, test, type Locator, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

const BARS = [
  { page: 'Actions', route: '/#/actions', name: 'Change tasks' },
  { page: 'Snapshot', route: '/#/advanced', name: 'Overview sections' },
  { page: 'Fleet', route: '/#/manage-databases', name: 'Fleet sections' },
  { page: 'Notifications', route: '/#/notifications', name: 'Notification sections' },
  { page: 'Settings', route: '/#/settings', name: 'Settings sections' },
]

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

// expectSelected checks that tab is the one selected, focused tab stop and
// that the page shows the panel it controls.
async function expectSelected(page: Page, tab: Locator) {
  await expect(tab).toHaveAttribute('aria-selected', 'true')
  await expect(tab).toBeFocused()
  await expect(tab).toHaveAttribute('tabindex', '0')
  const id = await tab.getAttribute('id')
  const panelId = await tab.getAttribute('aria-controls')
  expect(id).toBeTruthy()
  const panel = page.locator(`[id="${panelId}"]`)
  await expect(panel).toHaveAttribute('role', 'tabpanel')
  await expect(panel).toHaveAttribute('aria-labelledby', id as string)
  await expect(page.getByRole('tabpanel')).toHaveCount(1)
}

for (const bar of BARS) {
  test(`${bar.page}: the tab bar is a tablist of tabs only`, async ({ page }) => {
    await page.goto(bar.route)
    const list = page.getByRole('tablist', { name: bar.name })
    await expect(list).toBeVisible()
    const children = list.locator(':scope > *')
    const count = await children.count()
    expect(count).toBeGreaterThan(1)
    for (let i = 0; i < count; i += 1) {
      await expect(children.nth(i)).toHaveAttribute('role', 'tab')
    }
    await expect(list.locator('[role="tab"][aria-selected="true"]')).toHaveCount(1)
    await expect(list.locator('[role="tab"][tabindex="0"]')).toHaveCount(1)
  })

  test(`${bar.page}: Arrow, Home and End move the selected tab and its panel`,
    async ({ page }) => {
      await page.goto(bar.route)
      const tabs = page.getByRole('tablist', { name: bar.name }).getByRole('tab')
      const last = (await tabs.count()) - 1
      await tabs.first().click()
      await expectSelected(page, tabs.first())
      await page.keyboard.press('ArrowRight')
      await expectSelected(page, tabs.nth(1))
      await expect(tabs.first()).toHaveAttribute('aria-selected', 'false')
      await page.keyboard.press('End')
      await expectSelected(page, tabs.nth(last))
      await page.keyboard.press('End')
      await expectSelected(page, tabs.nth(last))
      await page.keyboard.press('ArrowRight')
      await expectSelected(page, tabs.first())
      await page.keyboard.press('ArrowLeft')
      await expectSelected(page, tabs.nth(last))
      await page.keyboard.press('Home')
      await expectSelected(page, tabs.first())
    })
}
