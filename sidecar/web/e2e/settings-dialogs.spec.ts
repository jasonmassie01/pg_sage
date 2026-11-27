// settings-dialogs.spec.ts — keyboard checks for the Settings dialogs
// (dashboard-10c). Both the configuration review and the trust escalation
// confirm run on the shared native <dialog>, so focus stays inside while
// open and returns to the opener on Escape. Every /api call is mocked.

import { test, expect, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { readFileSync } from 'node:fs'

type KeyMeta = { min?: number, max?: number, type?: string }
// config_meta.json is the bundle's per-key contract (min, max, type).
const META = JSON.parse(readFileSync(
  new URL('../src/generated/config_meta.json', import.meta.url), 'utf8')) as
  Record<string, KeyMeta>

// fillEveryInput gives each number input of the tab a new value inside its
// key's range: Save refuses an out-of-range edit before the review dialog
// opens (config-02b), so 900 + i only where the range allows it.
async function fillEveryInput(page: Page) {
  const inputs = page.locator('input[type="number"]')
  const count = await inputs.count()
  for (let i = 0; i < count; i += 1) {
    const input = inputs.nth(i)
    const key = (await input.getAttribute('data-testid') ?? '').replace(/^setting-/, '')
    await input.fill(inRangeValue(META[key] ?? {}, 900 + i, await input.inputValue()))
  }
  return count
}

function inRangeValue(meta: KeyMeta, wanted: number, current: string) {
  const min = meta.min ?? 0
  const max = meta.max ?? Number.MAX_SAFE_INTEGER
  const whole = /^u?int/.test(meta.type ?? 'int')
  let v = wanted <= max ? wanted : (whole ? Math.floor((min + max) / 2) : (min + max) / 2)
  if (String(v) === current) v = whole ? v - 1 : v / 2
  return String(Math.max(min, v))
}

async function openAdvancedTab(page: Page, tab: string) {
  await page.goto('#/settings')
  await page.getByTestId('settings-mode-toggle').click()
  await page.getByTestId(`settings-tab-${tab}`).click()
}

function focusInside(page: Page, testId: string) {
  return page.evaluate(id => {
    const dialog = document.querySelector(`[data-testid="${id}"]`)
    return Boolean(dialog && dialog.contains(document.activeElement))
  }, testId)
}

async function tabTwentyTimesInside(page: Page, testId: string) {
  for (let i = 0; i < 20; i += 1) {
    await page.keyboard.press('Tab')
    expect(await focusInside(page, testId), `focus left the dialog after Tab ${i + 1}`)
      .toBe(true)
  }
}

test.describe('Settings dialogs by keyboard', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
    await page.addInitScript(() => {
      localStorage.removeItem('pg_sage_settings_mode')
    })
  })

  test('the review dialog keeps focus inside and Escape returns it to Save', async ({ page }) => {
    await openAdvancedTab(page, 'trust-safety')
    await page.getByTestId('setting-safety.cpu_ceiling_pct').fill('70')
    const save = page.getByTestId('settings-save')
    await save.focus()
    await page.keyboard.press('Enter')

    const dialog = page.getByRole('dialog', { name: /review configuration changes/i })
    await expect(dialog).toBeVisible()
    await expect(page.getByTestId('config-diff-cancel')).toBeFocused()
    await expect(page.getByTestId('config-diff-after-safety.cpu_ceiling_pct'))
      .toHaveText('70')
    await tabTwentyTimesInside(page, 'config-diff-modal')

    await page.keyboard.press('Escape')
    await expect(dialog).toBeHidden()
    await expect(save).toBeFocused()
  })

  test('the review dialog sends one PUT with the edited key', async ({ page }) => {
    const bodies: unknown[] = []
    await page.route('**/api/v1/config/global', route => {
      if (route.request().method() === 'PUT') {
        bodies.push(route.request().postDataJSON())
        return route.fulfill({ status: 200, json: { ok: true } })
      }
      return route.fallback()
    })
    await openAdvancedTab(page, 'trust-safety')
    await page.getByTestId('setting-safety.cpu_ceiling_pct').fill('70')
    await page.getByTestId('settings-save').click()
    await page.getByTestId('config-diff-confirm').click()
    await expect(page.getByTestId('config-diff-modal')).toBeHidden()
    expect(bodies).toEqual([{ 'safety.cpu_ceiling_pct': '70' }])
  })

  test('raising trust to autonomous asks for the phrase and traps focus', async ({ page }) => {
    await openAdvancedTab(page, 'trust-safety')
    const select = page.getByTestId('setting-trust.level')
    await expect(select).toHaveValue('advisory')
    await select.focus()
    await select.selectOption('autonomous')

    const dialog = page.getByRole('dialog', { name: /confirm autonomy change/i })
    await expect(dialog).toBeVisible()
    await expect(page.getByTestId('trust-level-cancel')).toBeFocused()
    await tabTwentyTimesInside(page, 'trust-level-confirm')
    await page.keyboard.press('Escape')
    await expect(dialog).toBeHidden()
    await expect(select).toBeFocused()
    await expect(select).toHaveValue('advisory')

    await select.selectOption('autonomous')
    const confirm = page.getByTestId('trust-level-confirm-btn')
    await expect(confirm).toBeDisabled()
    await page.getByTestId('trust-level-phrase').fill('Autonomous')
    await expect(confirm).toBeDisabled()
    await page.getByTestId('trust-level-phrase').fill('autonomous')
    await confirm.click()
    await expect(dialog).toBeHidden()
    await expect(select).toHaveValue('autonomous')
  })

  test('a long diff scrolls inside the dialog, not the page', async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 420 })
    await openAdvancedTab(page, 'analyzer')
    const count = await fillEveryInput(page)
    expect(count).toBeGreaterThanOrEqual(5)
    await page.getByTestId('settings-save').click()
    const rows = page.getByTestId('config-diff-rows')
    await expect(rows.getByTestId(/^config-diff-row-/)).toHaveCount(count)

    const overflow = await rows.evaluate(el => el.scrollHeight > el.clientHeight)
    expect(overflow).toBe(true)
    const pageScrollBefore = await page.evaluate(() => window.scrollY)
    await rows.hover()
    for (let i = 0; i < 6; i += 1) await page.mouse.wheel(0, 600)
    await expect.poll(() => rows.evaluate(el => el.scrollTop)).toBeGreaterThan(0)
    expect(await page.evaluate(() => window.scrollY)).toBe(pageScrollBefore)
    await expect(page.getByTestId('config-diff-confirm')).toBeInViewport()
  })

  test('a long diff can be reached and scrolled by keyboard', async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 420 })
    await openAdvancedTab(page, 'analyzer')
    await fillEveryInput(page)
    await page.getByTestId('settings-save').click()
    const rows = page.getByRole('region', { name: /configuration changes/i })
    await expect(page.getByTestId('config-diff-cancel')).toBeFocused()

    await page.keyboard.press('Shift+Tab')
    await expect(rows).toBeFocused()
    for (let i = 0; i < 8; i += 1) await page.keyboard.press('ArrowDown')
    await expect.poll(() => rows.evaluate(el => el.scrollTop)).toBeGreaterThan(0)
    await page.keyboard.press('Shift+Tab')
    await expect(page.getByTestId('config-diff-confirm')).toBeFocused()
    await page.keyboard.press('Tab')
    await expect(rows).toBeFocused()
  })
})
