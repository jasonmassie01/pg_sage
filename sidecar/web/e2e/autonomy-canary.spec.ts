// autonomy-canary.spec.ts (dashboard-14): the Earned autonomy page during a
// running fleet canary (executor-04 CHECK-04): each database's state in the
// canary, "unknown" where none is stored, and the run's state moving on the
// next poll. Payloads: e2e/mocks/autonomy.ts.
// No concurrency tests: each test drives one page.

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  CANARY_RUN_ID, canaryRun, instanceRecord, routeAutonomy,
} from './mocks/autonomy'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

const detailOf = (instances: unknown[], state = 'canary') => () => ({
  status: 200, json: { run: canaryRun(state, 1), instances },
})

test('executor-04 CHECK-04: a running canary shows each database\'s state',
  async ({ page }) => {
    const asked = await routeAutonomy(page, {
      runs: () => [canaryRun('canary', 1)],
      detail: detailOf([instanceRecord('reporting', 1, 'awaiting_verdict'),
        instanceRecord('billing', 2, 'applying')]),
    })
    await page.goto('/#/advanced/autonomy')
    const run = page.getByTestId('rollout-run')
    await expect(run).toHaveCount(1)
    await expect(run).toContainText('orders: canary, 1 applied')
    await expect(run.getByTestId('rollout-instance'))
      .toHaveText(['reporting: awaiting_verdict', 'billing: applying'])
    expect(asked).toContain(`/api/v1/sre/autonomy/rollouts/${CANARY_RUN_ID}`)
  })

test('a database with no stored status reads "unknown", not blank', async ({ page }) => {
  await routeAutonomy(page, {
    runs: () => [canaryRun('canary', 0)],
    detail: detailOf([instanceRecord('reporting', 1, '')]),
  })
  await page.goto('/#/advanced/autonomy')
  await expect(page.getByTestId('rollout-instance')).toHaveText(['reporting: unknown'])
})

test('a canary with no instance records yet reads "unknown"', async ({ page }) => {
  await routeAutonomy(page, { runs: () => [canaryRun('canary', 0)], detail: detailOf([]) })
  await page.goto('/#/advanced/autonomy')
  await expect(page.getByTestId('rollout-instances-unknown'))
    .toHaveText('Databases: unknown (no database has reported a state yet)')
  await expect(page.getByTestId('rollout-instance')).toHaveCount(0)
})

test('a run with no stored state reads "unknown"', async ({ page }) => {
  await routeAutonomy(page, { runs: () => [canaryRun('', 0)] })
  await page.goto('/#/advanced/autonomy')
  await expect(page.getByTestId('rollout-run')).toContainText('orders: unknown, 0 applied')
})

test('a failed detail read says so and keeps the run line', async ({ page }) => {
  await routeAutonomy(page, {
    runs: () => [canaryRun('canary', 1)],
    detail: () => ({ status: 500, json: { error: 'rollout store unavailable' } }),
  })
  await page.goto('/#/advanced/autonomy')
  await expect(page.getByTestId('rollout-instances-error'))
    .toContainText('rollout store unavailable')
  await expect(page.getByTestId('rollout-run')).toContainText('orders: canary, 1 applied')
})

test('the canary moving to complete changes the page on the next poll', async ({ page }) => {
  await page.clock.install()
  let state = 'canary'
  await routeAutonomy(page, {
    runs: () => [canaryRun(state, state === 'canary' ? 1 : 2)],
    detail: detailOf([instanceRecord('reporting', 1, 'awaiting_verdict')]),
  })
  await page.goto('/#/advanced/autonomy')
  const run = page.getByTestId('rollout-run')
  await expect(run).toContainText('orders: canary, 1 applied')
  await expect(run.getByTestId('rollout-instance')).toHaveText(['reporting: awaiting_verdict'])

  state = 'complete'
  // The canary list polls every 60 s (AutonomyPage useAPI interval).
  await page.clock.fastForward(60_000)
  await expect(run).toContainText('orders: complete, 2 applied')
  await expect(run).not.toContainText('canary,')
})

test('no canary runs shows the empty state', async ({ page }) => {
  await routeAutonomy(page, { runs: () => [] })
  await page.goto('/#/advanced/autonomy')
  await expect(page.getByText('No canary rollouts.')).toBeVisible()
  await expect(page.getByTestId('rollout-run')).toHaveCount(0)
})
