// settings-badges.spec.ts (dashboard-18): the Settings masked-override
// badge (config-03, reports/t1/config-03.md:150 MANUAL) and the lifecycle
// banner wording (config-07, reports/t1/config-07.md:138 MANUAL).
// Payloads: e2e/mocks/configbadges.ts on dashboard-15's settings.ts.
// No concurrency tests: one admin reads one Settings page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  LIFECYCLE_BANNER, MASKED_TRUST_TITLE, badgeBody, maskedModel, maskedTrust, routeBadges,
} from './mocks/configbadges'

// label is the Settings label holding one config key's badges.
function label(page: Page, key: string) {
  return page.locator('div.py-2').filter({ has: page.getByTestId(`setting-${key}`) })
    .locator('label')
}

const badge = (page: Page, key: string, text: string) =>
  label(page, key).getByText(text, { exact: true })

// openMonitoring opens Settings on the simple Monitoring tab, which holds
// trust.level and the analyzer window.
async function openMonitoring(page: Page) {
  await page.goto('/#/settings')
  await page.getByTestId('settings-tab-monitoring').click()
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.removeItem('pg_sage_settings_mode'))
})

test('config-03: a UI override masking the file value is badged with both values',
  async ({ page }) => {
    await routeBadges(page, () => badgeBody([maskedTrust]))
    await openMonitoring(page)
    const masked = badge(page, 'trust.level', 'masked')
    await expect(masked).toBeVisible()
    await expect(masked).toHaveAttribute('title', MASKED_TRUST_TITLE)
    await expect(badge(page, 'trust.level', 'override')).toBeVisible()
    // The control shows the effective, more restrictive value.
    await expect(page.getByTestId('setting-trust.level')).toHaveValue('observation')
    // Only the masked key is badged.
    await expect(page.getByText('masked', { exact: true })).toHaveCount(1)
  })

test('config-03: the LLM model shows its masked badge on AI & Alerts', async ({ page }) => {
  await routeBadges(page, () => badgeBody([maskedModel]))
  await page.goto('/#/settings')
  await page.getByTestId('settings-tab-ai-alerts').click()
  const model = page.locator('label').filter({ hasText: /^Model/ })
  await expect(model.getByText('masked', { exact: true })).toHaveAttribute('title',
    /sets gpt-4o; a UI override \(user 1, .*\) sets gpt-4o-mini\. Effective: gpt-4o-mini\.$/)
})

test('no masked overrides, or a null list, draws no masked badge', async ({ page }) => {
  let body: unknown = badgeBody([])
  await routeBadges(page, () => body)
  await openMonitoring(page)
  await expect(page.getByTestId('setting-trust.level')).toBeVisible()
  await expect(page.getByText('masked', { exact: true })).toHaveCount(0)

  body = { ...badgeBody([]), masked_by_override: null }
  await page.reload()
  await page.getByTestId('settings-tab-monitoring').click()
  await expect(page.getByTestId('setting-trust.level')).toBeVisible()
  await expect(page.getByText('masked', { exact: true })).toHaveCount(0)
})

test('a masked entry without a key is ignored', async ({ page }) => {
  await routeBadges(page, () => ({ ...badgeBody([maskedTrust]),
    masked_by_override: [{ ...maskedTrust, key: '' }] }))
  await openMonitoring(page)
  await expect(page.getByTestId('setting-trust.level')).toBeVisible()
  await expect(page.getByText('masked', { exact: true })).toHaveCount(0)
})

test('config-07: the banner says which saved fields apply without a restart',
  async ({ page }) => {
    await routeBadges(page, () => badgeBody([]))
    await page.goto('/#/settings')
    await expect(page.getByTestId('settings-lifecycle-banner')).toHaveText(LIFECYCLE_BANNER)
    // The per-field badges agree with the banner.
    await page.getByTestId('settings-tab-monitoring').click()
    await expect(page.getByTestId('lifecycle-trust.level')).toHaveText('live')
    await expect(page.getByTestId('lifecycle-analyzer.unused_index_window_days'))
      .toHaveText('restart')
    await expect(page.getByTestId('settings-restart-hint')).toContainText(
      'Restart the sidecar through your supervisor')
    await page.getByTestId('settings-tab-ai-alerts').click()
    await expect(page.getByTestId('lifecycle-llm.endpoint')).toHaveText('live')
    await expect(page.getByTestId('lifecycle-llm.endpoint')).toHaveAttribute('title',
      'Saved values rebuild the component in place; no restart.')
  })

test('an unknown restart reason from a newer server keeps the generic wording',
  async ({ page }) => {
    await routeBadges(page, () => badgeBody([], {
      restart_reason: 'quantum_reconfigure',
      pending: [{ key: 'collector.interval_seconds', active: 60, desired: 120,
        lifecycle: 'quantum_reconfigure', restart_reason: 'quantum_reconfigure' }],
      pending_restart: ['collector.interval_seconds'],
    }))
    await page.goto('/#/settings')
    await expect(page.getByTestId('settings-lifecycle-banner')).toHaveText(LIFECYCLE_BANNER)
    const panel = page.getByTestId('settings-pending-panel')
    await expect(panel).toContainText('1 setting waits for restart')
    await expect(panel.getByTestId('pending-collector.interval_seconds'))
      .toHaveText(/^collector\.interval_seconds\s*running 60 → after restart 120$/)
    await expect(page.getByText('quantum_reconfigure')).toHaveCount(0)
  })
