// cases-presentation.spec.ts (dashboard-13): the case card's basis
// sentence and recurrence line (cases-07 CHECK-27) and the evidence age of
// a resumed investigation (sre-02 CHECK-03), in the real bundle with real
// routing and clicks. Payloads: e2e/mocks/cases.ts.
// No concurrency tests: one page reads its own mocked responses.

import { expect, test, type Locator, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  DISK_BASIS, incidentCase, investigationRecord, routeCases, routeFleet,
  routeInvestigations,
} from './mocks/cases'
import { SAMPLE_INTERVAL_S } from '../src/pages/cases/staleEvidence.js'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeFleet(page)
})

// attention is the card's "Reason for attention" line.
const attention = (card: Locator) =>
  card.locator('p').filter({ hasText: /^Reason for attention:/ })

async function showCases(page: Page, cases: unknown[]) {
  await routeCases(page, () => ({ cases, total: cases.length }))
  await page.goto('/#/cases')
  await expect(page.getByTestId('cases-page')).toBeVisible()
}

test('cases-07 CHECK-27: a case shows the basis sentence and the recurrence line',
  async ({ page }) => {
    await showCases(page, [incidentCase()])
    const card = page.getByRole('article')
    await expect(card).toHaveCount(1)
    await expect(attention(card)).toHaveText(
      `Reason for attention: About 3 days until the disk is full (${DISK_BASIS})`)
    await expect(card.getByTestId('case-recurrence'))
      .toHaveText('Recurrence: 3rd occurrence; previous resolved 2026-11-02')
  })

test('a basis sentence containing <b> renders as text, not markup', async ({ page }) => {
  const basis = '<b>disk</b> grows 2 GB/h'
  await showCases(page, [incidentCase({
    urgency: { assessed: true, time_to_impact_seconds: 7200,
      impact: 'until the disk is full', basis },
  })])
  const card = page.getByRole('article')
  await expect(attention(card)).toHaveText(
    `Reason for attention: About 2 hours until the disk is full (${basis})`)
  await expect(card.locator('b')).toHaveCount(0)
})

test('a case with no recurrence shows no recurrence line', async ({ page }) => {
  await showCases(page, [incidentCase({ recurrence: undefined })])
  const card = page.getByRole('article')
  await expect(attention(card)).toContainText('About 3 days until the disk is full')
  await expect(card.getByTestId('case-recurrence')).toHaveCount(0)
})

test('a recurrence without history says the history is unavailable', async ({ page }) => {
  await showCases(page, [incidentCase({
    recurrence: { previous_id: '2a0b', history_unavailable: true },
  })])
  await expect(page.getByTestId('case-recurrence'))
    .toHaveText('Recurrence: Recurring incident; occurrence history unavailable')
})

async function openInvestigation(page: Page) {
  const panel = page.getByTestId('investigation-panel')
  await panel.getByTestId('investigation-toggle').click()
  await expect(panel.getByTestId('investigation-detail')).toBeVisible()
  return panel
}

test('sre-02 CHECK-03: a resumed investigation shows its evidence age', async ({ page }) => {
  await page.clock.install()
  let record = investigationRecord('paused')
  await routeInvestigations(page, () => record)
  const resumes: unknown[] = []
  await page.route('**/api/v1/databases/orders/investigations/inv-7/resume', route => {
    resumes.push(route.request().postDataJSON())
    record = investigationRecord('collecting')
    return route.fulfill({ json: { ok: true } })
  })
  await showCases(page, [incidentCase()])
  const panel = await openInvestigation(page)
  await expect(panel.getByTestId('investigation-state')).toHaveText('Paused')
  await expect(panel.getByTestId('investigation-evidence-age')).toHaveCount(0)

  // paused -> running: the detail refetches and is live (Stop is offered).
  await panel.getByTestId('investigation-resume').click()
  await expect(panel.getByTestId('investigation-stop')).toBeVisible()
  expect(resumes).toEqual([{ version: 3 }])
  await expect(panel.getByTestId('investigation-evidence-age')).toHaveCount(0)

  // running -> concluded on 25-minute-old evidence, seen on the next live poll.
  record = investigationRecord('concluded', {
    evidence_newest_at: '2026-11-12T09:35:00Z', evidence_age_s: 1500,
  })
  await page.clock.fastForward(5_000)
  await expect(panel.getByTestId('investigation-evidence-age'))
    .toHaveText('Concluded on evidence 25 min old.')
  await expect(panel.getByTestId('investigation-stop')).toHaveCount(0)
})

// The label appears only for evidence older than one sample interval: at
// the interval the evidence was collected during the run itself.
for (const [age, label] of [
  [SAMPLE_INTERVAL_S, null],
  [SAMPLE_INTERVAL_S + 1, `Concluded on evidence ${SAMPLE_INTERVAL_S + 1} s old.`],
] as const) {
  test(`evidence aged ${age} s (sample interval ${SAMPLE_INTERVAL_S} s) ` +
    `${label ? 'shows' : 'hides'} the age label`, async ({ page }) => {
    const record = investigationRecord('concluded', { evidence_age_s: age })
    await routeInvestigations(page, () => record)
    await showCases(page, [incidentCase()])
    const panel = await openInvestigation(page)
    await expect(panel.getByTestId('investigation-likely')).toBeVisible()
    if (label) {
      await expect(panel.getByTestId('investigation-evidence-age')).toHaveText(label)
    } else {
      await expect(panel.getByTestId('investigation-evidence-age')).toHaveCount(0)
    }
  })
}
