// nav-admin.spec.ts (dashboard-18): the Admin nav group per role and the
// keyboard shortcuts sheet (dashboard-06, reports/t2/dashboard-06.md:90
// MANUAL "the Admin group and the shortcuts sheet"). The nav is derived
// from src/routes.js; the sheet's rows from src/components/keymap.js.
// No concurrency tests: one signed-in user drives one page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs, mockAuthMe } from './fixtures'

const OPERATE = ['Inbox', 'Issues', 'Actions', 'Ledger', 'Workload', 'SLOs', 'Autonomy', 'Facts',
  'Ask Sage']
const ADMIN = ['Fleet', 'Users', 'Notifications', 'Alert log', 'Settings', 'MCP tokens',
  'Agents']
const SHORTCUTS = [
  ['Ctrl+K', 'Open or close the command palette'],
  ['?', 'Show these keyboard shortcuts'],
  ['Esc', 'Close the palette or this sheet'],
  ['↑↓', 'Move through palette results'],
  ['Enter', 'Run the selected command'],
]

async function signInAs(page: Page, role: string) {
  await page.route('**/api/v1/auth/me', route =>
    route.fulfill({ json: { ...mockAuthMe, role } }))
}

// group is the links under one nav heading of the desktop sidebar.
function group(page: Page, heading: string) {
  return page.getByTestId('sidebar-desktop').getByText(heading, { exact: true })
    .locator('xpath=..').locator('a[data-testid^="nav-"]')
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('dashboard-06: an admin sees the Admin group, in registry order', async ({ page }) => {
  await signInAs(page, 'admin')
  await page.goto('/#/')
  await expect(group(page, 'Operate')).toHaveText(OPERATE)
  await expect(group(page, 'Admin')).toHaveText(ADMIN)
  // Advanced is collapsed until opened, then lists its pages.
  const advanced = page.getByTestId('sidebar-desktop').getByRole('button', { name: 'Advanced' })
  await expect(advanced).toHaveAttribute('aria-expanded', 'false')
  await expect(page.getByTestId('nav-runbooks')).toHaveCount(0)
  await advanced.click()
  await expect(page.locator('#advanced-navigation a')).toHaveText(
    ['Runbooks', 'Snapshot & metrics'])
})

test('an operator sees only the Admin pages a reviewer may open', async ({ page }) => {
  await signInAs(page, 'operator')
  await page.goto('/#/')
  // alerting-06c opened Notifications (its Log tab) to operators.
  await expect(group(page, 'Admin')).toHaveText(['Notifications', 'Alert log', 'Agents'])
  await expect(page.getByTestId('nav-settings')).toHaveCount(0)
})

test('a viewer has no Admin group at all', async ({ page }) => {
  await signInAs(page, 'viewer')
  await page.goto('/#/')
  await expect(group(page, 'Operate')).toHaveText(OPERATE)
  await expect(page.getByTestId('sidebar-desktop').getByText('Admin', { exact: true }))
    .toHaveCount(0)
})

test('clicking an Admin link opens its page and marks it current', async ({ page }) => {
  await signInAs(page, 'admin')
  await page.goto('/#/')
  const link = page.getByTestId('sidebar-desktop').getByTestId('nav-users')
  await link.click()
  await expect(page).toHaveURL(/#\/users$/)
  await expect(link).toHaveAttribute('aria-current', 'page')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Users')
})

test('dashboard-06: ? opens the shortcuts sheet listing the keymap', async ({ page }) => {
  await page.goto('/#/')
  await expect(page.getByTestId('nav-inbox')).toBeVisible()
  await page.keyboard.press('?')
  const sheet = page.getByTestId('shortcuts-sheet')
  await expect(sheet).toBeVisible()
  await expect(sheet).toHaveAttribute('aria-label', 'Keyboard shortcuts')
  const rows = sheet.getByTestId('shortcut-row')
  await expect(rows).toHaveCount(SHORTCUTS.length)
  for (const [i, [keys, description]] of SHORTCUTS.entries()) {
    await expect(rows.nth(i).locator('dt')).toHaveText(keys)
    await expect(rows.nth(i).locator('dd')).toHaveText(description)
  }
  await expect(sheet.getByRole('button', { name: 'Close' })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(sheet).toHaveCount(0)
})

test('the sheet closes on Close and on a click outside it', async ({ page }) => {
  await page.goto('/#/')
  await expect(page.getByTestId('nav-inbox')).toBeVisible()
  await page.keyboard.press('?')
  await page.getByTestId('shortcuts-sheet').getByRole('button', { name: 'Close' }).click()
  await expect(page.getByTestId('shortcuts-sheet')).toHaveCount(0)
  await page.keyboard.press('?')
  await page.getByTestId('shortcuts-sheet').click({ position: { x: 5, y: 5 } })
  await expect(page.getByTestId('shortcuts-sheet')).toHaveCount(0)
})

test('? typed into a text field does not open the sheet', async ({ page }) => {
  await page.goto('/#/')
  await expect(page.getByTestId('nav-inbox')).toBeVisible()
  await page.keyboard.press('Control+k')
  const input = page.getByTestId('command-palette-input')
  await expect(input).toBeFocused()
  await page.keyboard.type('?')
  await expect(input).toHaveValue('?')
  await expect(page.getByTestId('shortcuts-sheet')).toHaveCount(0)
})

test('the palette\'s Keyboard shortcuts command opens the sheet', async ({ page }) => {
  await page.goto('/#/')
  await expect(page.getByTestId('nav-inbox')).toBeVisible()
  await page.keyboard.press('Control+k')
  await page.getByTestId('command-palette-input').fill('keyboard')
  await page.getByTestId('command-help').click()
  await expect(page.getByTestId('command-palette')).toHaveCount(0)
  await expect(page.getByTestId('shortcuts-sheet').getByTestId('shortcut-row'))
    .toHaveCount(SHORTCUTS.length)
})
