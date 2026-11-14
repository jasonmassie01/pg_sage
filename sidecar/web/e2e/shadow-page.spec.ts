// shadow-page.spec.ts (dashboard-18): the Shadow Mode panel on Settings,
// rebuilt on the shadow ledger (earned-04, reports/t2/earned-04.md:79
// MANUAL "dashboard view of the rebuilt page"). Payloads: e2e/mocks/shadow.ts.
// No concurrency tests: the panel is a read-only render of two reads.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  SHADOW_EMPTY, classSummary, decision, routeLedger, routeTrustLedger, trustRow,
  type LedgerAnswer,
} from './mocks/shadow'

const report = (page: Page) => page.getByTestId('shadow-mode-report')
const row = (page: Page, db: string, cls: string) => page.getByTestId(`shadow-class-${db}-${cls}`)

const twoDatabases: LedgerAnswer = { json: {
  databases: [
    { database: 'primary', summary: [
      classSummary('index_create', 'tuning',
        { total: 5, correct: 4, incorrect: 1, counted: 3 }, 135),
      classSummary('vacuum', 'maintenance', { total: 2, pending: 2 }, 0),
      classSummary('retire_query_hint', 'tuning', { total: 1, unscored: 1 }, null),
    ], decisions: [] },
    // A database with nothing shadowed is left out, not drawn as an empty table.
    { database: 'replica', summary: [], decisions: [] },
  ],
  meaning: 'What pg_sage would have done below each earned level.',
} }

async function openShadow(page: Page) {
  await page.goto('/#/settings')
  await expect(report(page).getByRole('heading', { name: 'Shadow Mode' })).toBeVisible()
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await page.addInitScript(() => localStorage.removeItem('pg_sage_settings_mode'))
})

test('earned-04: per class the decisions, their scores and the potential minutes',
  async ({ page }) => {
    await routeLedger(page, () => twoDatabases)
    await routeTrustLedger(page, () => ({ json: { enforced: true, databases: [] } }))
    await openShadow(page)
    const index = row(page, 'primary', 'index_create')
    for (const [key, value] of [['total', '5'], ['pending', '0'], ['correct', '4'],
      ['incorrect', '1'], ['counted', '3']]) {
      await expect(index.getByTestId(`shadow-${key}`)).toHaveText(value)
    }
    await expect(index.getByTestId('shadow-potential')).toHaveText('135 min potential')
    await expect(index).toContainText('tuning')
    // Modelled with nothing counted is 0, not "not modelled".
    await expect(row(page, 'primary', 'vacuum').getByTestId('shadow-potential'))
      .toHaveText('0 min potential')
    await expect(row(page, 'primary', 'vacuum').getByTestId('shadow-pending')).toHaveText('2')
    await expect(row(page, 'primary', 'retire_query_hint').getByTestId('shadow-potential'))
      .toHaveText('not modelled')
    await expect(report(page).locator('thead th')).toHaveText(['Class', 'Decisions', 'Pending',
      'Correct', 'Incorrect', 'Counted', 'Potential minutes'])
    await expect(page.locator('[data-testid^="shadow-class-replica-"]')).toHaveCount(0)
    await expect(page.getByTestId('shadow-empty')).toHaveCount(0)
  })

test('a class opens what pg_sage would have done, its SQL shown as text', async ({ page }) => {
  const sql = 'CREATE INDEX CONCURRENTLY "<img src=x onerror=alert(1)>" ON public.orders (id)'
  const reads = await routeLedger(page, () => twoDatabases,
    cls => (cls === 'index_create' ? [decision(31, sql)] : []))
  await routeTrustLedger(page, () => ({ json: { enforced: true, databases: [] } }))
  await openShadow(page)
  const index = row(page, 'primary', 'index_create')
  await index.getByRole('button', { name: 'What pg_sage would have done' }).click()
  const item = index.getByTestId('shadow-decision-31')
  await expect(item.locator('code').first()).toHaveText(sql)
  await expect(item).toContainText('predicted mean_exec_time -35% (hypopg)')
  await expect(item).toContainText('gate: shadow (below_earned_level) · if trusted: execute')
  await expect(item).toContainText('correct · verification · counted · mean_exec_time fell 41%')
  await expect(page.locator('img[src="x"]')).toHaveCount(0)
  expect(reads).toEqual(['?database=primary&class=index_create&limit=20'])

  const vacuum = row(page, 'primary', 'vacuum')
  await vacuum.getByRole('button', { name: 'What pg_sage would have done' }).click()
  await expect(vacuum).toContainText('No shadow decisions for this class yet.')
})

const emptyLedger: LedgerAnswer = { json: { databases: [{ database: 'primary',
  summary: [], decisions: [] }] } }

for (const [name, trust, reason] of [
  ['enforcement off', { json: { enforced: false, databases: [] } },
    'sre.autonomy.enforce is off: the trust ledger grants no levels, so no class is below ' +
    'one and nothing is shadowed.'],
  ['every class at L3', { json: { enforced: true, databases: [{ database: 'primary',
    rows: [trustRow('index_create', 'L3'), trustRow('vacuum', 'L4')] }] } },
  'Every class is already at L3 (unattended), so none is below its earned level and ' +
    'nothing is shadowed.'],
  ['a class below L3', { json: { enforced: true, databases: [{ database: 'primary',
    rows: [trustRow('index_create', 'L3'), trustRow('vacuum', 'L1')] }] } },
  'Nothing recorded yet: pg_sage records a shadow decision when it withholds an action ' +
    'below a class\'s earned level.'],
  ['an unreadable trust ledger', { status: 503, json: { error: 'trust store unavailable' } },
    'The trust ledger could not be read (trust store unavailable (503)); pg_sage shadows ' +
    'only classes below their earned level in that ledger.'],
] as const) {
  test(`an empty ledger with ${name} says why nothing is shadowed`, async ({ page }) => {
    await routeLedger(page, () => emptyLedger)
    await routeTrustLedger(page, () => trust as LedgerAnswer)
    await openShadow(page)
    const empty = page.getByTestId('shadow-empty')
    await expect(empty).toContainText(SHADOW_EMPTY)
    await expect(empty).toContainText(reason)
  })
}

test('a failed ledger read is an error with Retry, not an empty ledger', async ({ page }) => {
  let answer: LedgerAnswer = { status: 500, json: { error: 'shadow ledger unavailable' } }
  await routeLedger(page, () => answer)
  await routeTrustLedger(page, () => ({ json: { enforced: true, databases: [] } }))
  await page.goto('/#/settings')
  const alert = page.getByRole('alert').filter({ hasText: 'shadow ledger unavailable' })
  await expect(alert).toBeVisible()
  await expect(page.getByTestId('shadow-empty')).toHaveCount(0)

  answer = twoDatabases
  await alert.getByRole('button', { name: 'Retry' }).click()
  await expect(row(page, 'primary', 'index_create').getByTestId('shadow-potential'))
    .toHaveText('135 min potential')
})
