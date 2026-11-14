// actions-decisions.spec.ts (dashboard-14): the Actions > Decisions tab's
// "Budget: next frees at" (policy-07 CHECK-15) and the scheduled approvals
// on the dashboard (policy-04 CHECK-19, dashboard half) in the real bundle.
// Payloads: e2e/mocks/actions.ts. The Slack half of policy-04 CHECK-19 is an
// OWNER ACTION in dashboard-17.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  budgetPark, decisionsBody, pendingQueued, routeActions, routeDecisions, scheduledRow,
  windowBlock,
} from './mocks/actions'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test.describe('Decisions tab', () => {
  test('policy-07 CHECK-15: a budget park shows "Budget: next frees at"', async ({ page }) => {
    await routeDecisions(page, () => ({
      status: 200, json: decisionsBody([budgetPark, windowBlock]),
    }))
    await page.goto('/#/actions')
    await page.getByTestId('actions-tab-decisions').click()
    const rows = page.getByTestId('decision-row')
    await expect(rows).toHaveCount(2)
    await expect(rows.nth(0)).toContainText('rate limit exceeded (next frees at 18:00)')
    await expect(rows.nth(0)).toContainText('seen 3 times')
    await expect(rows.nth(0).getByTestId('decision-budget'))
      .toHaveText('Budget: next frees at 18:00')
    await expect(rows.nth(1)).toContainText('outside maintenance window')
    await expect(rows.nth(1).getByTestId('decision-budget')).toHaveCount(0)
    await expect(rows.nth(1).getByRole('link', { name: 'Approval request #9' }))
      .toHaveAttribute('href', '#/actions?database=orders')
  })

  test('the verdict filter asks for that verdict', async ({ page }) => {
    const asked = await routeDecisions(page, url => ({
      status: 200,
      json: decisionsBody(url.searchParams.get('verdict') === 'parked' ? [budgetPark]
        : [budgetPark, windowBlock]),
    }))
    await page.goto('/#/actions')
    await page.getByTestId('actions-tab-decisions').click()
    await expect(page.getByTestId('decision-row')).toHaveCount(2)
    await page.getByTestId('decisions-verdict').selectOption('parked')
    await expect(page.getByTestId('decision-row')).toHaveCount(1)
    expect(asked).toContain('/api/v1/decisions')
    expect(asked.at(-1)).toBe('/api/v1/decisions?verdict=parked')
  })

  test('an empty ledger says nothing is withheld', async ({ page }) => {
    await routeDecisions(page, () => ({ status: 200, json: decisionsBody([]) }))
    await page.goto('/#/actions')
    await page.getByTestId('actions-tab-decisions').click()
    await expect(page.getByTestId('decisions-empty')).toContainText('Nothing is withheld')
    await expect(page.getByTestId('decision-row')).toHaveCount(0)
  })

  test('a 503 shows the error, not an empty list', async ({ page }) => {
    await routeDecisions(page, () => ({
      status: 503, json: { error: 'decision ledger unavailable' },
    }))
    await page.goto('/#/actions')
    await page.getByTestId('actions-tab-decisions').click()
    await expect(page.getByTestId('decisions-tab'))
      .toContainText('decision ledger unavailable')
    await expect(page.getByTestId('decisions-empty')).toHaveCount(0)
    await expect(page.getByTestId('decision-row')).toHaveCount(0)
  })
})

test.describe('Scheduled approvals', () => {
  test('policy-04 CHECK-19: a scheduled approval shows when it runs, with Cancel',
    async ({ page }) => {
      await routeActions(page, {
        ledger: () => [], pending: () => ({ pending: [], scheduled: [scheduledRow()] }),
      })
      await page.goto('/#/actions')
      await page.getByTestId('actions-tab-pending').click()
      const box = page.getByTestId('scheduled-approvals')
      await expect(box).toContainText('Approved, waiting for the maintenance window')
      const expected = await page.evaluate(() => new Date('2026-11-17T07:00:00Z')
        .toLocaleString(undefined, { weekday: 'short', year: 'numeric', month: 'short',
          day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }))
      await expect(box).toContainText(`Scheduled for ${expected}`)
      await expect(box).toContainText('orders')
      await expect(box).toContainText('CREATE INDEX CONCURRENTLY i ON public.t (c)')
      await expect(box.getByRole('button', { name: 'Cancel schedule of action 41' }))
        .toBeEnabled()
    })

  test('Cancel returns the approval to pending', async ({ page }) => {
    let scheduled = true
    const posts = await routeActions(page, {
      ledger: () => [],
      pending: () => scheduled ? { pending: [], scheduled: [scheduledRow()] }
        : { pending: [pendingQueued()], scheduled: [] },
      unschedule: () => {
        scheduled = false
        return { status: 200, json: { ok: true, status: 'pending' } }
      },
    })
    await page.goto('/#/actions')
    await page.getByTestId('actions-tab-pending').click()
    await page.getByRole('button', { name: 'Cancel schedule of action 41' }).click()
    await expect(page.getByTestId('scheduled-approvals')).toHaveCount(0)
    await expect(page.getByTestId('pending-actions-table')).toContainText('orders')
    expect(posts).toEqual([
      { url: '/api/v1/actions/41/unschedule?database=orders', body: null },
    ])
  })

  test('a refused Cancel shows the error and keeps the schedule', async ({ page }) => {
    await routeActions(page, {
      ledger: () => [], pending: () => ({ pending: [], scheduled: [scheduledRow()] }),
      unschedule: () => ({ status: 409, json: { error: 'the action already ran' } }),
    })
    await page.goto('/#/actions')
    await page.getByTestId('actions-tab-pending').click()
    await page.getByRole('button', { name: 'Cancel schedule of action 41' }).click()
    const box = page.getByTestId('scheduled-approvals')
    await expect(box.getByRole('alert')).toHaveText('the action already ran')
    await expect(box).toContainText('Scheduled for')
  })

  test('an approval with no run_after waits for the next maintenance window',
    async ({ page }) => {
      await routeActions(page, {
        ledger: () => [],
        pending: () => ({ pending: [], scheduled: [scheduledRow({ run_after: null })] }),
      })
      await page.goto('/#/actions')
      await page.getByTestId('actions-tab-pending').click()
      await expect(page.getByTestId('scheduled-approvals'))
        .toContainText('Scheduled for the next maintenance window')
    })

  test('no scheduled approvals shows the empty state', async ({ page }) => {
    await routeActions(page, { ledger: () => [], pending: () => ({ pending: [], scheduled: [] }) })
    await page.goto('/#/actions')
    await page.getByTestId('actions-tab-pending').click()
    await expect(page.getByText('Nothing needs your approval in this view.')).toBeVisible()
    await expect(page.getByTestId('scheduled-approvals')).toHaveCount(0)
  })
})
