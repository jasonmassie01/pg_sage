// autonomy-page.spec.ts (ux-06b, decision D6): one Autonomy page with one
// scale per class. Both old paths render it (CHECK-04); each class shows
// its sentence and why; waiting promotions carry the one Approve; the
// history and shadow ledger sit collapsed below; the database form's one
// control writes both stored fields. Payloads: e2e/mocks/trust.ts.
// No concurrency tests: each test drives one page (the two-admin race is
// TestAutonomyModeDB_TwoAdminsApproveOnePromotion on PostgreSQL).

import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { routeTrust, selfRow, trustBody } from './mocks/trust'

const PENDING = { id: '44444444-4444-4444-8444-444444444444', family: 'tuning',
  class: 'index_create', from: 'L2', to: 'L3', expires_at: '2026-11-19T00:00:00Z' }

function modeRows() {
  return [
    { ...selfRow(false), mode: 'acts_then_tells',
      mode_because: ['earned level L3', 'tier3_safe is on'] },
    { ...selfRow(false), family: 'tuning', class: 'index_create', level: 'L2',
      effective: 'L2', mode: 'asks_first', mode_because: ['earned level L2'],
      pending: PENDING },
  ]
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

for (const path of ['/#/autonomy', '/#/trust', '/#/advanced/autonomy']) {
  test(`CHECK-04: ${path} renders the Autonomy page`, async ({ page }) => {
    await routeTrust(page, () => ({ json: trustBody(modeRows()) }))
    await page.goto(path)
    await expect(page.getByTestId('autonomy-hub')).toBeVisible()
    await expect(page.locator('h1')).toHaveText('Autonomy')
    await expect(page.getByTestId('autonomy-scale')).toContainText('One scale per class')
    await expect(page).toHaveTitle(/^Autonomy · pg_sage$/)
    const history = page.getByTestId('autonomy-history')
    if (path === '/#/advanced/autonomy') {
      await expect(history).toHaveAttribute('open', '')
    } else {
      await expect(history).not.toHaveAttribute('open', '')
    }
  })
}

test('each class says what pg_sage may do and why', async ({ page }) => {
  await routeTrust(page, () => ({ json: trustBody(modeRows()) }))
  await page.goto('/#/autonomy')
  const vacuum = page.getByTestId('trust-row-orders-hygiene-vacuum').getByTestId('trust-mode')
  await expect(vacuum).toHaveAttribute('data-mode', 'acts_then_tells')
  await expect(vacuum).toContainText('acts, then tells you')
  await expect(vacuum.getByTestId('trust-mode-because')).toContainText('tier3_safe is on')
  const create = page.getByTestId('trust-row-orders-tuning-index_create')
  await expect(create.getByTestId('trust-mode')).toContainText('asks first')
  await expect(create.getByTestId('trust-next')).toContainText('under Waiting promotions')
  await expect(page.getByTestId('autonomy-badge')).toHaveText('1 ask first · 1 act, then tell')
})

test('a waiting promotion has the one Approve, and approving re-reads', async ({ page }) => {
  let approved = false
  const posts: string[] = []
  await routeTrust(page, () => ({ json: trustBody(approved
    ? [modeRows()[0], { ...modeRows()[1], pending: undefined, level: 'L3' }]
    : modeRows()) }))
  await page.route(url => url.pathname.endsWith('/approve'), route => {
    posts.push(route.request().url())
    approved = true
    return route.fulfill({ json: { ok: true } })
  })
  await page.goto('/#/autonomy')
  await expect(page.getByRole('button', { name: 'Approve' })).toHaveCount(1)
  await page.getByTestId('autonomy-waiting-orders-tuning-index_create')
    .getByRole('button', { name: 'Approve' }).click()
  await expect(page.getByTestId('autonomy-waiting-empty')).toContainText('Nothing waits')
  expect(posts).toHaveLength(1)
  expect(posts[0]).toContain(`/api/v1/sre/autonomy/proposals/${PENDING.id}/approve`)
})

test('a second approval shows the conflict and the row as it now is', async ({ page }) => {
  let reads = 0
  await routeTrust(page, () => {
    reads += 1
    return { json: trustBody(modeRows()) }
  })
  await page.route(url => url.pathname.endsWith('/approve'), route =>
    route.fulfill({ status: 409, json: { error: 'autonomy proposal is not pending' } }))
  await page.goto('/#/autonomy')
  await expect(page.getByTestId('autonomy-waiting')).toBeVisible()
  const before = reads
  await page.getByRole('button', { name: 'Approve' }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'not pending' })).toBeVisible()
  await expect.poll(() => reads).toBeGreaterThan(before)
})

test('the shadow ledger opens on the Autonomy page, not Settings', async ({ page }) => {
  await routeTrust(page, () => ({ json: trustBody(modeRows()) }))
  await page.goto('/#/autonomy')
  const shadow = page.getByTestId('autonomy-shadow')
  await shadow.locator('summary').click()
  await expect(shadow).toHaveAttribute('open', '')
  await page.goto('/#/settings')
  await expect(page.getByTestId('shadow-mode-report')).toHaveCount(0)
})

const CHOICES: [string, string, string][] = [
  ['watch', 'observation', 'manual'], ['ask', 'advisory', 'approval'],
  ['tell', 'advisory', 'auto'], ['window', 'autonomous', 'auto'],
]

test('the database form writes both fields for each of the four choices',
  async ({ page }) => {
    await page.goto('/#/manage-databases')
    await page.getByTestId('add-database-button').click()
    await expect(page.getByTestId('db-autonomy-watch')).toBeChecked()
    for (const [id, trust, mode] of CHOICES) {
      await page.getByTestId(`db-autonomy-${id}`).check()
      await expect(page.getByTestId('db-trust-level')).toHaveValue(trust)
      await expect(page.getByTestId('db-execution-mode')).toHaveValue(mode)
      await expect(page.getByTestId('db-autonomy-custom')).toHaveCount(0)
    }
    await page.getByTestId('db-autonomy-raw').locator('summary').click()
    await page.getByTestId('db-execution-mode').selectOption('approval')
    await expect(page.getByTestId('db-autonomy-custom'))
      .toContainText('trust_level=autonomous, execution_mode=approval')
  })
