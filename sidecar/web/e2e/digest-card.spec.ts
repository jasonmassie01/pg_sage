// digest-card.spec.ts (ux-02b): home shows the latest digest from
// GET /api/v1/briefings/latest with its headline and five sections in
// order, how each channel took it, and "No digest yet" on a 404.
// Payloads: e2e/mocks/value.ts (copies of the briefing.Record shape).
// No concurrency tests: a read-only card one person views.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { NO_BRIEFING, digestBriefing, routeBriefing } from './mocks/value'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('home renders the digest card with the five sections', async ({ page }) => {
  await routeBriefing(page, () => ({
    json: { briefings: [digestBriefing], without_briefing: [], unavailable: [] },
  }))
  await page.goto('/#/')
  const card = page.getByTestId('digest-card')
  await expect(card.getByRole('heading', { name: 'Digest' })).toBeVisible()
  await expect(card.getByTestId('digest-headline-orders'))
    .toHaveText('6 changes, 5 verified, 1 reverted, 2 need you')
  const sections = card.getByTestId(/^digest-section-/)
  await expect(sections).toHaveCount(5)
  await expect(sections.nth(0)).toContainText('Did')
  await expect(sections.nth(1)).toContainText('Verified or reverted')
  await expect(sections.nth(2)).toContainText('Saved')
  await expect(sections.nth(3)).toContainText('Needs you')
  await expect(sections.nth(4)).toContainText('Could not do and why')
  await expect(card.getByTestId('digest-delivery-orders'))
    .toContainText('config:slack:briefing: sent')
})

test('home says no digest yet with the server reason', async ({ page }) => {
  await routeBriefing(page, () => ({ status: 404, json: { error: NO_BRIEFING } }))
  await page.goto('/#/')
  await expect(page.getByTestId('digest-empty')).toContainText('No digest yet')
  await expect(page.getByTestId('digest-empty')).toContainText('briefing.schedule')
})
