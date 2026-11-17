// plan-review.spec.ts (ux-03): the plan review surface inside Actions in
// the real bundle, mocked (e2e/mocks/plans.ts copies plan-03a's handler
// golden). The halted-plan story runs from the case link to the Inbox
// item; the real engine run is plan-02e's G-T5 integration test.
// No concurrency tests here: one approval in flight is
// PlanApproval.test.jsx ("approve clicked twice sends one request").

import { expect, test } from '@playwright/test'
import { mockAllAPIs, mockCases } from './fixtures'
import { inboxItem, routeInbox } from './mocks/inbox'
import { PLAN_HASH, contentChanged, fivePlan, routePlans, withRuntimes } from './mocks/plans'

const GOAL = 'cut blocks per call on the order lookups'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('halted plan: case link, approve all, halt on stage 3 of 5, then the Inbox item',
  async ({ page }) => {
    await page.clock.install({ time: new Date('2026-11-16T10:00:00Z') })
    const base = fivePlan()
    let gets = 0
    let approved = false
    const story = [
      withRuntimes(base, 'approved', {}),
      withRuntimes(base, 'running', { 1: { state: 'running' } }),
      withRuntimes(base, 'halted', {
        1: { state: 'kept', verdict: 'improved' }, 2: { state: 'kept', verdict: 'no_change' },
        3: { state: 'reverted', verdict: 'regressed',
          state_reason: 'blocks per call rose 40%' } }),
    ]
    const decisions = await routePlans(page, () => {
      if (!approved) return base
      return story[Math.min(gets++, story.length - 1)]
    }, () => { approved = true; return null })
    await page.route('**/api/v1/cases**', route => route.fulfill({ json: { ...mockCases,
      cases: mockCases.cases.map(c => ({ ...c, database_name: 'orders',
        plans: [{ id: 7, goal: GOAL }] })) } }))
    const inbox = { items: [] as ReturnType<typeof inboxItem>[], rows: [] }
    await routeInbox(page, inbox)

    await page.goto('/#/cases')
    await page.getByRole('link', { name: `Part of plan: ${GOAL}` }).first().click()
    await expect(page).toHaveURL(/#\/actions\?tab=plans&plan=7&database=orders$/)
    await expect(page.getByTestId('actions-tab-plans')).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByTestId('plan-header')).toContainText(GOAL)
    await expect(page.getByTestId(/^plan-stage-\d+$/)).toHaveCount(5)

    await page.getByRole('button', { name: 'Approve all 5 stages' }).click()
    expect(decisions).toHaveLength(1)
    expect(new URL(decisions[0].url).pathname + new URL(decisions[0].url).search)
      .toBe('/api/v1/plans/7/approve?database=orders')
    expect(decisions[0].body).toEqual({ plan_hash: PLAN_HASH, mode: 'all' })

    await page.clock.fastForward(5_000)
    await expect(page.getByTestId('plan-stage-status-1')).toHaveText('Running')
    await page.clock.fastForward(5_000)
    const halt = page.getByTestId('plan-halted')
    await expect(halt).toContainText('Halted at stage 3: blocks per call rose 40%')
    await expect(halt).toContainText('Reverted: stage 3')
    await expect(halt).toContainText('Kept: stages 1, 2')
    await expect(halt).toContainText('Never started: stages 4, 5')

    inbox.items = [inboxItem('plan_approval', '7/3', { database: 'orders', title: GOAL,
      why: 'step 3 is reverted', href: '#/actions?tab=plans&plan=7&database=orders',
      source: 'sage.plan', actionable: false })]
    await page.goto('/#/')
    const item = page.getByTestId('inbox-item-plan_approval-7/3')
    await expect(item).toContainText(GOAL)
    await item.getByRole('link', { name: 'Open' }).click()
    await expect(page.getByTestId('plan-halted')).toContainText('Halted at stage 3')
  })

test('a stale hash voids the approval and reloads the plan', async ({ page }) => {
  let gets = 0
  await routePlans(page, () => { gets++; return fivePlan() },
    () => ({ status: 409, json: contentChanged }))
  await page.goto('/#/actions?tab=plans&plan=7&database=orders')
  await page.getByRole('button', { name: 'Approve all 5 stages' }).click()
  await expect(page.getByTestId('plan-approval').getByRole('alert'))
    .toContainText('This plan changed after approval; approval voided')
  await expect.poll(() => gets).toBeGreaterThan(1)
})

test('a 30-stage plan stays readable at 375 px with no page scroll', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 })
  const base = fivePlan()
  const steps = Array.from({ length: 30 }, (_, i) => ({ ...base.plan.steps[0],
    id: 100 + i, ordinal: i + 1 }))
  await routePlans(page, { ...base, plan: { ...base.plan, steps, edges: [] },
    coverage: steps.map(s => ({ covered: false, needs_own_approval: false,
      ordinal: s.ordinal, step_id: s.id })) })
  await page.goto('/#/actions?tab=plans&plan=7&database=orders')
  const list = page.getByTestId('plan-stages')
  await expect(list.getByTestId(/^plan-stage-\d+$/)).toHaveCount(30)
  const box = await list.boundingBox()
  expect(box?.height ?? 0).toBeLessThan(812)
  const scroll = await list.evaluate(el => ({ inner: el.scrollHeight, outer: el.clientHeight }))
  expect(scroll.inner).toBeGreaterThan(scroll.outer)
  const pageWidth = await page.evaluate(() => document.documentElement.scrollWidth)
  expect(pageWidth).toBeLessThanOrEqual(375)
  await expect(list.getByTestId('plan-stage-30')).toContainText('Stage 30')
})
