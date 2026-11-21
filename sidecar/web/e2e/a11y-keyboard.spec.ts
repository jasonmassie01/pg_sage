// a11y-keyboard.spec.ts (ux-12): the pages a person decides on, driven by
// the keyboard only after load: Tab reaches each page's controls, the tab
// bars answer the arrow keys, an Autonomy section opens with Enter, a
// confirmation dialog takes focus and gives it back on Escape, and on a
// phone (under 768 px) the Inbox item and plan approval rows are the
// sticky action bar, while at 768 px they are not.
// Payloads: the default mocks in e2e/fixtures.ts (admin user), plus
// e2e/mocks/inbox.ts, plans.ts and agents.ts.
// No concurrency tests: each test drives one page in one browser context.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { inboxItem, routeInbox } from './mocks/inbox'
import { fivePlan, routePlans } from './mocks/plans'
import { ADMIN1, AgentsMock, installAgentsMock } from './mocks/agents'

const MAX_TABS = 120

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

// tabTo presses Tab until the focused element matches selector and fails
// with the count when it never does, so a control off the Tab order is a
// failure, not a hang.
async function tabTo(page: Page, selector: string) {
  for (let i = 0; i < MAX_TABS; i += 1) {
    await page.keyboard.press('Tab')
    const hit = await page.evaluate(s => document.activeElement?.matches(s) ?? false,
      selector)
    if (hit) return i + 1
  }
  throw new Error(`Tab never reached ${selector} in ${MAX_TABS} presses`)
}

const focusedText = (page: Page) =>
  page.evaluate(() => document.activeElement?.textContent?.trim() || '')

async function arrowsMoveTabs(page: Page, list: string) {
  const bar = page.getByRole('tablist', { name: list })
  await expect(bar).toBeVisible()
  await tabTo(page, `[role="tablist"][aria-label="${list}"] [role="tab"]`)
  const first = await focusedText(page)
  await page.keyboard.press('ArrowRight')
  const second = await focusedText(page)
  expect(second).not.toBe(first)
  await expect(bar.getByRole('tab', { selected: true })).toHaveText(second)
  // Home selects the bar's first tab, which is not always the landing tab
  // (Changes opens on Waiting on you when approvals wait).
  await page.keyboard.press('Home')
  const firstTab = bar.getByRole('tab').first()
  await expect(firstTab).toHaveAttribute('aria-selected', 'true')
  await expect(firstTab).toBeFocused()
  await page.keyboard.press('Tab')
  const inPanel = await page.evaluate(() => {
    const panel = document.querySelector('[role="tabpanel"]')
    return Boolean(panel && panel.contains(document.activeElement))
  })
  expect(inPanel).toBe(true)
}

function inboxItems() {
  return { items: [
    inboxItem('change_approval', '42', { title: 'Add index on orders(customer_id)' }),
    inboxItem('fact', '7', { title: 'idx_legacy belongs to billing', risk: 'safe' }),
  ], rows: [] }
}

test('Inbox: Tab reaches an item and Enter opens its review', async ({ page }) => {
  await routeInbox(page, inboxItems())
  await page.goto('/#/')
  const item = page.getByTestId('inbox-item-change_approval-42')
  await expect(item).toBeVisible()
  await tabTo(page, '[data-testid="inbox-item-change_approval-42"] button')
  expect(await focusedText(page)).toBe('Review')
  await page.keyboard.press('Enter')
  await expect(item.getByTestId('approval-approve')).toBeVisible()
  await tabTo(page, '[data-testid="approval-approve"]')
})

test('Changes: the tab bar answers the arrow keys and Tab enters the panel',
  async ({ page }) => {
    await page.goto('/#/changes')
    await arrowsMoveTabs(page, 'Change tasks')
  })

test('Fleet: the tab bar answers the arrow keys and reaches Health & readiness',
  async ({ page }) => {
    await page.goto('/#/manage-databases')
    await arrowsMoveTabs(page, 'Fleet sections')
    const bar = page.getByRole('tablist', { name: 'Fleet sections' })
    await bar.getByRole('tab', { selected: true }).focus()
    await page.keyboard.press('End')
    for (let i = 0; i < 6; i += 1) {
      if (/Health/.test(await focusedText(page))) break
      await page.keyboard.press('ArrowLeft')
    }
    await expect(bar.getByRole('tab', { selected: true })).toContainText('Health')
  })

test('Agents: the arrow keys reach MCP tokens; the kill dialog takes and returns focus',
  async ({ page }) => {
    await installAgentsMock(page, new AgentsMock(), ADMIN1)
    await page.goto('/#/agents')
    await expect(page.getByTestId('agents-table')).toBeVisible()
    await arrowsMoveTabs(page, 'Agents sections')
    await tabTo(page, '[data-testid="agents-kill-open"]')
    await page.keyboard.press('Enter')
    const dialog = page.getByRole('dialog', { name: 'Kill agents' })
    await expect(dialog).toBeVisible()
    const inside = await page.evaluate(() => {
      const d = document.querySelector('[data-testid="agents-kill-dialog"]')
      return Boolean(d && d !== document.activeElement && d.contains(document.activeElement))
    })
    expect(inside).toBe(true)
    await page.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(page.getByTestId('agents-kill-open')).toBeFocused()
  })

test('Autonomy: Tab reaches a collapsed section and Enter opens it', async ({ page }) => {
  await page.goto('/#/autonomy')
  const history = page.getByTestId('autonomy-history')
  await expect(history).toBeVisible()
  await expect(history).not.toHaveAttribute('open', '')
  await tabTo(page, '[data-testid="autonomy-history"] > summary')
  await page.keyboard.press('Enter')
  await expect(history).toHaveAttribute('open', '')
})

const position = (page: Page, testId: string) => page.getByTestId(testId).first()
  .evaluate(el => getComputedStyle(el).position)

for (const [width, expected] of [[767, 'sticky'], [768, 'static'], [375, 'sticky']] as const) {
  test(`at ${width} px the Inbox item and plan approval rows are ${expected}`,
    async ({ page }) => {
      await page.setViewportSize({ width, height: 812 })
      await routeInbox(page, inboxItems())
      await page.goto('/#/')
      await expect(page.getByTestId('inbox-decision-bar').first()).toBeVisible()
      expect(await position(page, 'inbox-decision-bar')).toBe(expected)
      await routePlans(page, () => fivePlan())
      await page.goto('/#/changes?tab=plans&plan=7&database=orders')
      await expect(page.getByTestId('plan-approval-actions')).toBeVisible()
      expect(await position(page, 'plan-approval-actions')).toBe(expected)
    })
}
