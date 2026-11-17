// actions-keyboard.spec.ts (dashboard-14): keyboard-only rollback from the
// Actions ledger (dashboard-10 CHECK-13, the Chromium half) and the MCP
// token revoke dialog (dashboard-10b). jsdom has no top layer, so this is
// where Tab containment and focus return are checked. After page load the
// specs use page.keyboard only: locator.focus() places the caret on the
// first control, as a Tab walk would, and no mouse event is sent.
// The Firefox half of CHECK-13 is an OWNER ACTION in dashboard-17.
// No concurrency tests: each test drives one page and one dialog.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  executedRow, rollbackBody, rollbackReason, routeActions, routeTokens,
} from './mocks/actions'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

function activeTestId(page: Page) {
  return page.evaluate(() => document.activeElement?.getAttribute('data-testid') || '')
}

function focusInside(page: Page, testId: string) {
  return page.evaluate(id => {
    const dialog = document.querySelector(`[data-testid="${id}"]`)
    return Boolean(dialog && dialog !== document.activeElement
      && dialog.contains(document.activeElement))
  }, testId)
}

// tabTo presses Tab until the control with testId has focus.
async function tabTo(page: Page, testId: string, limit = 25) {
  for (let i = 0; i < limit; i += 1) {
    if (await activeTestId(page) === testId) return
    await page.keyboard.press('Tab')
  }
  expect(await activeTestId(page), `Tab never reached ${testId}`).toBe(testId)
}

// typeReason tabs to the required rollback reason (ux-01b) and types it.
async function typeReason(page: Page) {
  await tabTo(page, 'confirm-dialog-reason')
  await page.keyboard.type(rollbackReason)
}

// openRollbackByKeyboard reaches the row's rollback button and presses Enter.
async function openRollbackByKeyboard(page: Page) {
  await page.goto('/#/actions')
  const tab = page.getByTestId('actions-tab-executed')
  await tab.focus()
  await page.keyboard.press('Enter')
  const expander = page.getByTestId('row-expander').first()
  await expect(expander).toBeVisible()
  await expander.focus()
  await page.keyboard.press('Enter')
  await expect(expander).toHaveAttribute('aria-expanded', 'true')
  await tabTo(page, 'rollback-action-button')
  await page.keyboard.press('Enter')
  const dialog = page.getByRole('dialog', { name: /Roll back action 9/ })
  await expect(dialog).toBeVisible()
  return dialog
}

test('dashboard-10 CHECK-13: keyboard-only rollback keeps Tab in the dialog',
  async ({ page }) => {
    const posts = await routeActions(page, { ledger: () => [executedRow()] })
    const dialog = await openRollbackByKeyboard(page)
    await expect(page.getByTestId('confirm-dialog-cancel')).toBeFocused()
    for (let i = 0; i < 30; i += 1) {
      await page.keyboard.press('Tab')
      expect(await focusInside(page, 'confirm-dialog'), `focus left after Tab ${i + 1}`)
        .toBe(true)
    }
    await page.keyboard.press('Escape')
    await expect(dialog).toBeHidden()
    await expect(page.getByTestId('rollback-action-button')).toBeFocused()
    expect(posts, 'Escape sends nothing').toEqual([])
  })

test('Shift+Tab from the first control wraps to Confirm, and Tab wraps back',
  async ({ page }) => {
    await routeActions(page, { ledger: () => [executedRow()] })
    await openRollbackByKeyboard(page)
    await typeReason(page)
    const first = await page.evaluate(() => {
      const dialog = document.querySelector('[data-testid="confirm-dialog"]')
      const items = dialog?.querySelectorAll('button:not([disabled]), input:not([disabled])')
      const el = items?.[0] as HTMLElement | undefined
      el?.setAttribute('data-first-control', 'yes')
      return Boolean(el)
    })
    expect(first, 'the dialog has a first control').toBe(true)
    await tabTo(page, 'confirm-dialog-confirm')
    await page.keyboard.press('Tab')
    await expect(page.locator('[data-first-control="yes"]')).toBeFocused()
    await page.keyboard.press('Shift+Tab')
    await expect(page.getByTestId('confirm-dialog-confirm')).toBeFocused()
  })

test('Enter on Confirm sends the rollback once with today\'s body', async ({ page }) => {
  const posts = await routeActions(page, { ledger: () => [executedRow()] })
  const dialog = await openRollbackByKeyboard(page)
  await typeReason(page)
  await tabTo(page, 'confirm-dialog-confirm')
  await page.keyboard.press('Enter')
  await expect(dialog).toBeHidden()
  expect(posts).toEqual([
    { url: '/api/v1/actions/9/rollback?database=orders', body: rollbackBody },
  ])
})

test('a HIGH-tier rollback keeps Confirm disabled for a wrong phrase', async ({ page }) => {
  const posts = await routeActions(page, {
    ledger: () => [executedRow({ risk_tier: 'high' })],
  })
  await openRollbackByKeyboard(page)
  const phrase = page.getByTestId('confirm-dialog-phrase')
  const confirm = page.getByTestId('confirm-dialog-confirm')
  await expect(phrase).toBeFocused()
  await page.keyboard.type('order')
  await expect(confirm).toBeDisabled()
  // A disabled Confirm is never reached by Tab, so Enter cannot send it.
  for (let i = 0; i < 12; i += 1) {
    await page.keyboard.press('Tab')
    expect(await activeTestId(page)).not.toBe('confirm-dialog-confirm')
  }
  await tabTo(page, 'confirm-dialog-phrase')
  await page.keyboard.press('Enter')
  expect(posts, 'a wrong phrase sends nothing').toEqual([])

  // Tab into a text field selects its text; End keeps what was typed.
  await page.keyboard.press('End')
  await page.keyboard.type('s')
  await expect(phrase).toHaveValue('orders')
  await expect(confirm, 'the phrase alone is not enough: a reason is required').toBeDisabled()
  await typeReason(page)
  await expect(confirm).toBeEnabled()
  await tabTo(page, 'confirm-dialog-confirm')
  await page.keyboard.press('Enter')
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0].body).toEqual(rollbackBody)
})

test('a failed rollback keeps the dialog open with the error and focus inside',
  async ({ page }) => {
    const posts = await routeActions(page, {
      ledger: () => [executedRow()],
      rollback: () => ({ status: 400, json: { error: 'rollback refused: index in use' } }),
    })
    const dialog = await openRollbackByKeyboard(page)
    await typeReason(page)
    await tabTo(page, 'confirm-dialog-confirm')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('confirm-dialog-error'))
      .toHaveText('rollback refused: index in use')
    await expect(dialog).toBeVisible()
    expect(await focusInside(page, 'confirm-dialog'), 'focus stays in the dialog').toBe(true)
    await page.keyboard.press('Tab')
    expect(await focusInside(page, 'confirm-dialog')).toBe(true)
    expect(posts).toHaveLength(1)
  })

test('dashboard-10b: the token revoke dialog by keyboard', async ({ page }) => {
  const deletes = await routeTokens(page)
  const id = 'a1b2c3d4-0000-4000-8000-000000000002'
  await page.goto('/#/mcp-tokens')
  const revoke = page.getByTestId(`mcp-token-revoke-${id}`)
  await expect(revoke).toBeVisible()
  await revoke.focus()
  await page.keyboard.press('Enter')

  const dialog = page.getByRole('dialog', { name: 'Revoke MCP token "oncall-cursor"' })
  await expect(dialog).toBeVisible()
  await expect(page.getByTestId('confirm-dialog-cancel')).toBeFocused()
  for (let i = 0; i < 30; i += 1) {
    await page.keyboard.press('Tab')
    expect(await focusInside(page, 'confirm-dialog'), `focus left after Tab ${i + 1}`)
      .toBe(true)
  }
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await expect(revoke).toBeFocused()
  expect(deletes, 'Escape revokes nothing').toEqual([])

  await page.keyboard.press('Enter')
  await expect(dialog).toBeVisible()
  await tabTo(page, 'confirm-dialog-confirm')
  await page.keyboard.press('Enter')
  await expect(page.getByTestId(`mcp-token-status-${id}`)).toHaveText('revoked')
  expect(deletes).toEqual([`/api/v1/mcp/tokens/${id}`])
})
