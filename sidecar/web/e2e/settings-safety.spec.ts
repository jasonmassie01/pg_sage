// settings-safety.spec.ts (dashboard-15): the trust ceiling toggle on
// Settings > Trust & Safety (config-01 CHECK-06) and the pending-restart
// panel with the source and lifecycle badges (config-09 CHECK-28).
// Payloads: e2e/mocks/settings.ts.
// No concurrency tests: each test drives one page through one save at a time.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  configState, maskedCpu, pendingEntries, routeConfig,
} from './mocks/settings'

const TIER3 = 'trust.tier3_safe'

async function openTrustSafety(page: Page) {
  await page.goto('/#/settings')
  await page.getByTestId('settings-mode-toggle').click()
  await page.getByTestId('settings-tab-trust-safety').click()
}

// fieldRow is the Settings row holding one config key's control and badges.
function fieldRow(page: Page, key: string) {
  return page.locator('div.py-2').filter({ has: page.getByTestId(`setting-${key}`) })
}

function sourceBadge(page: Page, key: string, source: string) {
  return fieldRow(page, key).locator('label').getByText(source, { exact: true })
}

async function saveReviewed(page: Page) {
  await page.getByTestId('settings-save').click()
  await expect(page.getByTestId('config-diff-modal')).toBeVisible()
  await page.getByTestId('config-diff-confirm').click()
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.removeItem('pg_sage_settings_mode'))
})

test('config-01 CHECK-06: turning tier3_safe off sends one PUT and shows the override',
  async ({ page }) => {
    const state = configState()
    const { puts } = await routeConfig(page, state)
    await openTrustSafety(page)
    const toggle = page.getByTestId(`setting-${TIER3}`)
    await expect(toggle).toHaveText('Enabled')
    await expect(sourceBadge(page, TIER3, 'file/default')).toBeVisible()

    await toggle.click()
    await expect(toggle).toHaveText('Disabled')
    await expect(sourceBadge(page, TIER3, 'modified')).toBeVisible()
    await expect(page.getByTestId(`config-diff-after-${TIER3}`)).toHaveCount(0)
    await saveReviewed(page)

    await expect(page.getByTestId('config-diff-modal')).toBeHidden()
    // The body TestGlobalPutTrustCeilingIsAppliedNotPendingRestart sends.
    expect(puts).toEqual([{ [TIER3]: 'false', expected_generation: 1 }])
    await expect(toggle).toHaveText('Disabled')
    await expect(sourceBadge(page, TIER3, 'override')).toBeVisible()
    await expect(sourceBadge(page, TIER3, 'modified')).toHaveCount(0)
    await expect(page.getByText('Saved 1 global configuration change(s)')).toBeVisible()
    // A live trust ceiling is applied, so nothing waits for a restart.
    await expect(page.getByTestId('settings-pending-panel')).toHaveCount(0)
    await expect(page.getByTestId('settings-feedback')).toHaveCount(0)
  })

test('tier3_safe goes on, off and on again with two PUTs', async ({ page }) => {
  const state = configState()
  const { puts } = await routeConfig(page, state)
  await openTrustSafety(page)
  const toggle = page.getByTestId(`setting-${TIER3}`)

  await toggle.click()
  await saveReviewed(page)
  await expect(sourceBadge(page, TIER3, 'override')).toBeVisible()
  await expect(toggle).toHaveText('Disabled')

  await toggle.click()
  await expect(toggle).toHaveText('Enabled')
  await expect(page.getByTestId(`config-diff-after-${TIER3}`)).toHaveCount(0)
  await saveReviewed(page)
  await expect(page.getByTestId('config-diff-modal')).toBeHidden()
  await expect(toggle).toHaveText('Enabled')
  await expect(sourceBadge(page, TIER3, 'override')).toBeVisible()
  // Each PUT names the generation the page last read.
  expect(puts).toEqual([
    { [TIER3]: 'false', expected_generation: 1 },
    { [TIER3]: 'true', expected_generation: 2 },
  ])
  expect(state).toMatchObject({ tier3Safe: 'true', generation: 3 })
})

test('a refused save shows the server message beside the diff and changes nothing',
  async ({ page }) => {
    const state = configState()
    // Save refuses an out-of-range value before the PUT (config-02b), so the
    // refusal here is one only the server makes: a server whose range is
    // narrower than this bundle's config_meta.json (a version skew).
    const message = 'validate: trust.rollback_threshold_pct must be 0-30, got 40'
    // Multi-key saves are atomic (ValidateConfigOverride): one bad key
    // refuses the whole PUT (TestGlobalPutTrustCeilingRejectsNegativeThreshold).
    const { puts } = await routeConfig(page, state,
      () => ({ status: 400, json: { error: message } }))
    await openTrustSafety(page)
    const toggle = page.getByTestId(`setting-${TIER3}`)
    await toggle.click()
    await page.getByTestId('setting-trust.rollback_threshold_pct').fill('40')
    await saveReviewed(page)

    const dialog = page.getByTestId('config-diff-modal')
    await expect(dialog.getByTestId('config-diff-error')).toHaveText(message)
    await expect(dialog.getByTestId(`config-diff-after-${TIER3}`)).toHaveText('false')
    await expect(dialog.getByTestId('config-diff-after-trust.rollback_threshold_pct'))
      .toHaveText('40')
    expect(puts).toEqual([{ [TIER3]: 'false', 'trust.rollback_threshold_pct': '40',
      expected_generation: 1 }])
    expect(state).toMatchObject({ tier3Safe: 'true', tier3Source: 'file/default',
      generation: 1 })

    // Closing the dialog keeps the edit marked unsaved; Discard reverts it.
    await page.getByTestId('config-diff-cancel').click()
    await expect(dialog).toBeHidden()
    await expect(sourceBadge(page, TIER3, 'modified')).toBeVisible()
    await page.getByTestId('settings-discard').click()
    await expect(toggle).toHaveText('Enabled')
    await expect(sourceBadge(page, TIER3, 'file/default')).toBeVisible()
    await expect(page.getByTestId('settings-save')).toHaveCount(0)
    expect(puts).toHaveLength(1)
  })

test('config-09 CHECK-28: the pending-restart panel lists what waits, secrets hidden',
  async ({ page }) => {
    await routeConfig(page, configState({ pending: pendingEntries }))
    await page.goto('/#/settings')
    const panel = page.getByTestId('settings-pending-panel')
    await expect(panel).toBeVisible()
    await expect(panel).toContainText('2 settings wait for restart')
    const interval = panel.getByTestId('pending-collector.interval_seconds')
    await expect(interval.getByTestId('pending-active')).toHaveText('60')
    await expect(interval.getByTestId('pending-desired')).toHaveText('120')
    const secret = panel.getByTestId('pending-llm.api_key')
    await expect(secret).toContainText('value hidden (secret or not shown in the UI)')
    await expect(secret.getByTestId('pending-desired')).toHaveCount(0)
    await expect(panel).not.toContainText('sk-')
    // restart_supported false: the page says how to restart instead.
    await expect(page.getByTestId('settings-restart-hint')).toContainText('SAGE_SUPERVISED=1')
    await expect(page.getByTestId('settings-restart-btn')).toHaveCount(0)
  })

test('config-09 CHECK-28: source and lifecycle badges on Trust & Safety', async ({ page }) => {
  await routeConfig(page, configState({ tier3Source: 'override', masked: [maskedCpu] }))
  await openTrustSafety(page)
  await expect(sourceBadge(page, TIER3, 'override')).toBeVisible()
  await expect(page.getByTestId(`lifecycle-${TIER3}`)).toHaveText('live')
  const cpu = 'safety.cpu_ceiling_pct'
  await expect(sourceBadge(page, cpu, 'file/default')).toBeVisible()
  await expect(page.getByTestId(`lifecycle-${cpu}`)).toHaveText('restart')
  const masked = sourceBadge(page, cpu, 'masked')
  await expect(masked).toBeVisible()
  await expect(masked).toHaveAttribute('title', /sets 90; a UI override .* sets 80/)
  await expect(sourceBadge(page, 'trust.tier3_moderate', 'masked')).toHaveCount(0)
})

test('no pending settings and a supervisor: no panel, and Restart now is offered',
  async ({ page }) => {
    await routeConfig(page, configState({ pending: [], restartSupported: true }))
    await page.goto('/#/settings')
    await expect(page.getByTestId('settings-lifecycle-banner')).toBeVisible()
    await expect(page.getByTestId('settings-pending-panel')).toHaveCount(0)
    await expect(page.getByTestId('settings-restart-btn')).toHaveText('Restart now')
  })
