// actions-quickwins.spec.ts (ux-05a): Actions opens on Waiting on you
// when approvals wait (win 1), white-labelled decision buttons reach WCAG
// AA (win 2), the approval list answers j/k/a/r/s (win 24) and the card's
// action row is sticky below 768 px (win 25). The cards copy the shape
// approvalcard.Card serves (as approval-ladder.spec.ts does); the queue rows
// copy mockPendingActions, one per card, so the card view is complete.
// No concurrency tests here: the one-request guard is a unit test
// (ApprovalCards.keys.test.jsx), since Playwright key presses are serial.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs, mockPendingActions } from './fixtures'

const base = mockPendingActions.pending[0]
const ids = [51, 52, 53]
const queue = ids.map(id => ({ ...base, id, policy_decision: 'queue_for_approval',
  lifecycle_state: 'pending', blocked_reason: '', eligible: true }))

const cardFor = (id: number) => ({
  queue_id: id, database: base.database_name, title: `Analyze table t${id}`,
  action_type: 'analyze_table', status: 'pending', targets: [],
  evidence: [{ kind: 'finding', label: 'Finding #42', value: 'warning', ref: 'finding:42' }],
  rationale: { source: 'rule', text: 'Statistics are stale' }, predicted_effect: {},
  sql: `ANALYZE public.t${id}`,
  rollback: { class: 'no_rollback_needed', sql: '', note: 'Nothing to undo' },
  risk: { tier: 'moderate', blast_radius: 'one table', guardrails: [], post_checks: [] },
  why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
  proposed_at: '2026-11-16T09:00:00Z', expires_at: '2099-01-01T00:00:00Z',
  card_hash: `hash-${id}`,
})

type Post = { url: string, body: unknown }

async function routeQueue(page: Page, rows: unknown[]) {
  const posts: Post[] = []
  await page.route('**/api/v1/actions**', route => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/v1/actions/pending/count') {
      return route.fulfill({ json: { count: rows.length } })
    }
    if (url.pathname === '/api/v1/actions/pending') {
      return route.fulfill({ json: { pending: rows, scheduled: [], total: rows.length } })
    }
    return route.fulfill({ json: { actions: [], total: 0 } })
  })
  await page.route('**/api/v1/approvals**', route => {
    const req = route.request()
    const url = new URL(req.url())
    if (req.method() === 'POST') {
      posts.push({ url: url.pathname + url.search, body: req.postDataJSON() })
      return route.fulfill({ json: { ok: true, executed: true, verification_status: 'pending' } })
    }
    const cards = rows.map(r => cardFor((r as { id: number }).id))
    return route.fulfill({ json: { cards } })
  })
  return posts
}

// whiteTextFailures lists buttons with white text whose own fill is under
// 4.5:1, computed from the rendered colours.
function whiteTextFailures(page: Page) {
  return page.evaluate(() => {
    const lin = (v: number) => {
      const c = v / 255
      return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
    }
    const lum = (rgb: number[]) => 0.2126 * lin(rgb[0]) + 0.7152 * lin(rgb[1])
      + 0.0722 * lin(rgb[2])
    const parse = (s: string) => (s.match(/[\d.]+/g) || []).map(Number)
    const out: string[] = []
    for (const el of document.querySelectorAll('button')) {
      const style = getComputedStyle(el)
      const fg = parse(style.color)
      const bg = parse(style.backgroundColor)
      if (fg.slice(0, 3).join() !== '255,255,255' || bg.length < 3 || bg[3] === 0) continue
      const ratio = (1.05) / (lum(bg) + 0.05)
      if (ratio < 4.5) out.push(`${el.textContent?.trim()}: ${ratio.toFixed(2)}`)
    }
    return out
  })
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('opens on Waiting on you and approves the third card by keyboard',
  async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 })
    const posts = await routeQueue(page, queue)
    await page.goto('/#/actions')
    await expect(page.getByTestId('actions-tab-pending')).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByTestId('approval-card')).toHaveCount(3)
    await page.keyboard.press('j')
    await page.keyboard.press('j')
    await page.keyboard.press('a')
    const dialog = page.getByRole('dialog', { name: 'Approve: Analyze table t53' })
    await expect(dialog).toBeVisible()
    expect(posts).toEqual([])
    await dialog.getByTestId('approval-key-confirm-confirm').click()
    await expect(dialog).toBeHidden()
    expect(posts).toEqual([{ url: '/api/v1/approvals/53/approve?database=primary',
      body: { card_hash: 'hash-53' } }])
  })

test('one click from landing reaches the first approval', async ({ page }) => {
  const posts = await routeQueue(page, queue)
  await page.goto('/#/actions')
  await page.getByTestId('approval-approve').first().click()
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0].url).toBe('/api/v1/approvals/51/approve?database=primary')
})

test('opens on Proposed changes when nothing waits', async ({ page }) => {
  await routeQueue(page, [])
  await page.goto('/#/actions')
  await expect(page.getByTestId('actions-tab-recommendations'))
    .toHaveAttribute('aria-selected', 'true')
  await expect(page.getByTestId('actions-tab-pending')).toHaveAttribute('aria-selected', 'false')
})

test('a typed in the reject reason stays text', async ({ page }) => {
  const posts = await routeQueue(page, queue)
  await page.goto('/#/actions')
  await expect(page.getByTestId('approval-card')).toHaveCount(3)
  await page.keyboard.press('r')
  const reason = page.getByTestId('approval-reject-reason')
  await expect(reason).toBeFocused()
  await page.keyboard.type('a')
  await expect(reason).toHaveValue('a')
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(posts).toEqual([])
})

test('white-labelled buttons on the approval list reach 4.5:1', async ({ page }) => {
  await routeQueue(page, queue)
  await page.goto('/#/actions')
  await expect(page.getByTestId('approval-card')).toHaveCount(3)
  await page.getByTestId('approval-reject').first().click()
  await expect(page.getByTestId('approval-reject-confirm')).toBeVisible()
  expect(await whiteTextFailures(page)).toEqual([])
})

for (const [width, sticky] of [[375, true], [767, true], [768, false], [1280, false]] as const) {
  test(`the card action row is ${sticky ? '' : 'not '}sticky at ${width} px`,
    async ({ page }) => {
      await page.setViewportSize({ width, height: 800 })
      await routeQueue(page, queue)
      await page.goto('/#/actions')
      const bar = page.getByTestId('approval-actions').first()
      await expect(bar).toBeVisible()
      const style = await bar.evaluate(el => {
        const s = getComputedStyle(el)
        return { position: s.position, bottom: s.bottom }
      })
      expect(style.position).toBe(sticky ? 'sticky' : 'static')
      if (sticky) expect(style.bottom).toBe('0px')
    })
}
