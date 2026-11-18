// cases-lifecycle.spec.ts (dashboard-18): a case's recommendation
// lifecycle (cases-10, reports/t2/cases-10.md MANUAL: the next-step,
// expiry and retry lines) and an investigation in flight refreshing on the
// Cases page (sre-09, reports/t2/sre-09.md MANUAL, UI half; the live demo
// stack is dashboard-17 CHECK-18). Payloads: e2e/mocks/lifecycle.ts.
// No concurrency tests: one page reads its own mocked responses, and
// polling across pages is dashboard-16's hidden-tab spec.

import { expect, test, type Locator, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { incidentCase, investigationRecord, routeCases, routeFleet } from './mocks/cases'
import {
  NOW, ROOT_CAUSE, candidate, lifecycleCase, liveDetail, markExpired, plusMinutes,
  recommendation, routeLiveInvestigation, type LiveAnswer,
} from './mocks/lifecycle'

const nextStep = (card: Locator) => card.locator('p').filter({ hasText: /^Next step: / })

async function showCases(page: Page, cases: unknown[]) {
  await routeCases(page, () => ({ cases, total: cases.length }))
  await page.goto('/#/issues')
  await expect(page.getByTestId('cases-page')).toBeVisible()
}

// openDetails opens a card's "Technical evidence and proposed changes".
async function openDetails(card: Locator) {
  await card.getByTestId('case-technical-details').locator('summary').click()
  return card.getByTestId('case-technical-details')
}

test.describe('cases-10: the recommendation lifecycle on a case', () => {
  test.beforeEach(async ({ page }) => {
    await page.clock.setFixedTime(NOW)
    await mockAllAPIs(page)
    await routeFleet(page)
  })

  test('cases-10: a failed head shows the server next step, expiry and retry lines',
    async ({ page }) => {
      await showCases(page, [lifecycleCase('41', {
        recommendation: recommendation('failed', 'Retrying after 14:05 UTC',
          { next_attempt_at: plusMinutes(125) }),
        action_candidates: [candidate({ retrying_after: plusMinutes(125) })],
      })])
      const card = page.getByRole('article')
      await expect(nextStep(card)).toHaveText('Next step: Retrying after 14:05 UTC')
      const details = await openDetails(card)
      await expect(details.getByTestId('candidate-expiry'))
        .toHaveText('Evidence valid until 2026-11-12 13:00 UTC')
      await expect(details.getByTestId('candidate-retry'))
        .toHaveText('Retrying after 2026-11-12 14:05 UTC')
    })

  test('cases-10: a reverted head says what was applied and why it was reverted',
    async ({ page }) => {
      const step = 'pg_sage applied this on 2026-11-10 and reverted it: p95 latency rose 40%'
      await showCases(page, [lifecycleCase('41', {
        recommendation: recommendation('reverted', step,
          { applied_at: '2026-11-10T08:00:00Z', reason: 'p95 latency rose 40%' }),
        action_candidates: [candidate({ blocked_reason: 'recommendation_reverted' })],
      })])
      await expect(nextStep(page.getByRole('article'))).toHaveText(`Next step: ${step}`)
    })

  test('a proposal that expires exactly now shows as expired; one minute later does not',
    async ({ page }) => {
      await showCases(page, [
        lifecycleCase('41', { action_candidates: [markExpired(candidate({
          expires_at: NOW }))] }),
        lifecycleCase('42', { action_candidates: [markExpired(candidate({
          expires_at: plusMinutes(1) }))] }),
      ])
      const expired = page.getByRole('article').filter({ hasText: 'public.t41' })
      const fresh = page.getByRole('article').filter({ hasText: 'public.t42' })
      await expect(nextStep(expired))
        .toHaveText('Next step: Evidence expired; waiting for a fresh observation')
      await expect((await openDetails(expired)).getByTestId('candidate-expiry'))
        .toHaveText('Evidence expired at 2026-11-12 12:00 UTC')
      await expect(nextStep(fresh)).not.toContainText('Evidence expired')
      await expect((await openDetails(fresh)).getByTestId('candidate-expiry'))
        .toHaveText('Evidence valid until 2026-11-12 12:01 UTC')
    })

  test('a case with no recommendation lifecycle shows no lifecycle or retry line',
    async ({ page }) => {
      await showCases(page, [
        lifecycleCase('41', { action_candidates: [candidate({ expires_at: undefined })] }),
        // A head still proposed has nothing to report: its next step is not shown.
        lifecycleCase('42', { recommendation: recommendation('proposed',
          'Proposed; awaiting approval'), action_candidates: [candidate()] }),
      ])
      for (const id of ['41', '42']) {
        const card = page.getByRole('article').filter({ hasText: `public.t${id}` })
        await expect(nextStep(card)).toHaveText('Next step: A proposed change is attached. ' +
          'Review its evidence, restrictions and decision requirements in Actions.')
        const details = await openDetails(card)
        await expect(details.getByTestId('candidate-retry')).toHaveCount(0)
      }
      const noExpiry = page.getByRole('article').filter({ hasText: 'public.t41' })
      await expect(noExpiry.getByTestId('candidate-expiry')).toHaveCount(0)
    })

  test('an unknown state or an unreadable history is never shown as a known step',
    async ({ page }) => {
      await showCases(page, [
        lifecycleCase('41', { recommendation: recommendation('quarantined',
          'Recommendation state unknown (quarantined); review it before acting'),
        action_candidates: [candidate()] }),
        lifecycleCase('42', { recommendation_unknown: true,
          action_candidates: [candidate()] }),
      ])
      await expect(nextStep(page.getByRole('article').filter({ hasText: 'public.t41' })))
        .toHaveText('Next step: Recommendation state unknown. ' +
          'Review it in Actions before acting.')
      await expect(nextStep(page.getByRole('article').filter({ hasText: 'public.t42' })))
        .toHaveText('Next step: Whether pg_sage already tried this change could not be ' +
          'read. Review its history in Actions before acting.')
    })
})

test.describe('sre-09: a live investigation refreshes on the Cases page', () => {
  test.beforeEach(async ({ page }) => {
    await page.clock.install({ time: NOW })
    await mockAllAPIs(page)
    await routeFleet(page)
  })

  async function openPanel(page: Page) {
    const panel = page.getByTestId('investigation-panel')
    await panel.getByTestId('investigation-toggle').click()
    await expect(panel.getByTestId('investigation-detail')).toBeVisible()
    return panel
  }

  test('sre-09: running to concluded on the next poll, without a reload',
    async ({ page }) => {
      let record = investigationRecord('collecting')
      const counts = await routeLiveInvestigation(page, () => record, () => liveDetail(record))
      const navigations: string[] = []
      page.on('framenavigated', frame => {
        if (frame === page.mainFrame()) navigations.push(frame.url())
      })
      await showCases(page, [incidentCase()])
      const panel = await openPanel(page)
      await expect(panel.getByTestId('investigation-state')).toHaveText('Running')
      await expect(panel.getByTestId('investigation-likely'))
        .toContainText('No supported explanation.')

      record = investigationRecord('concluded')
      await page.clock.fastForward(5_000)
      await expect(panel.getByTestId('investigation-likely')).toContainText(
        `${ROOT_CAUSE.label} (${ROOT_CAUSE.subject}) - score 0.86, strong evidence`)
      await expect(panel.getByTestId('investigation-likely'))
        .toContainText('slot cdc_orders holds 41 GB of WAL')
      // The card's badge follows the list's 30 s poll.
      await page.clock.fastForward(30_000)
      await expect(panel.getByTestId('investigation-state')).toHaveText('Concluded')

      // Terminal: the detail is not fetched again (one extra GET is allowed
      // as polling switches off, sre-09 decision).
      const settled = counts.detail
      await page.clock.fastForward(30_000)
      await expect(panel.getByTestId('investigation-state')).toHaveText('Concluded')
      expect(counts.detail - settled).toBeLessThanOrEqual(1)
      expect(navigations).toHaveLength(1)
    })

  test('a failed refresh keeps the last answer and says so', async ({ page }) => {
    const record = investigationRecord('collecting')
    let answer: LiveAnswer = liveDetail(record)
    await routeLiveInvestigation(page, () => record, () => answer)
    await showCases(page, [incidentCase()])
    const panel = await openPanel(page)
    await expect(panel.getByTestId('investigation-refresh-failed')).toHaveCount(0)

    answer = { status: 503, json: { error: 'investigation store unavailable' } }
    await page.clock.fastForward(5_000)
    await expect(panel.getByTestId('investigation-refresh-failed')).toHaveText(
      'Refresh failed (investigation store unavailable (503)); showing the last answer.')
    await expect(panel.getByTestId('investigation-detail')).toBeVisible()
    await expect(panel.getByTestId('investigation-likely'))
      .toContainText('No supported explanation.')

    answer = liveDetail(record)
    await page.clock.fastForward(5_000)
    await expect(panel.getByTestId('investigation-refresh-failed')).toHaveCount(0)
  })
})
