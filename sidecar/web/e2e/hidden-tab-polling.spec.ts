// hidden-tab-polling.spec.ts (dashboard-16): dashboard-08 CHECK-INT. A hidden
// Actions tab sends no /api/v1/* request apart from /events over 120 s of
// mocked time, and on becoming visible it refreshes once per active query,
// not once per missed tick. document.hidden is driven by
// e2e/mocks/shell.ts controlVisibility; time by page.clock.
// No concurrency tests: visibility is per tab and each test drives one tab.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  controlVisibility, isEvents, logAPIRequests, routeEvents, setHidden,
} from './mocks/shell'

// resource is a request without its time window: a windowed query asks for
// a later from/to on each poll and is still one query.
function resource(path: string) {
  const u = new URL(path, 'http://x')
  u.searchParams.delete('from')
  u.searchParams.delete('to')
  return u.pathname + u.search
}

function polls(log: string[], from = 0) {
  return log.slice(from).filter(p => !isEvents(p)).map(resource)
}

function countBy(items: string[]) {
  const counts: Record<string, number> = {}
  for (const item of items) counts[item] = (counts[item] || 0) + 1
  return counts
}

// settle lets answers to requests already sent arrive (real time).
async function settle(page: Page) {
  await page.waitForTimeout(500)
}

async function openActions(page: Page) {
  await page.goto('/#/actions')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Actions')
  await expect(page.getByTestId('freshness-indicator')).toHaveAttribute('data-status', 'live')
  await settle(page)
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeEvents(page)
  await controlVisibility(page)
  await page.clock.install()
})

test('dashboard-08 CHECK-INT: a hidden Actions tab polls nothing but /events for 120 s',
  async ({ page }) => {
    const log = logAPIRequests(page)
    await openActions(page)
    const mounted = new Set(polls(log))
    expect(mounted).toContain('/api/v1/databases')
    expect(mounted).toContain('/api/v1/inbox/count')

    // Visible, the page polls: one 30 s round asks for the fleet again.
    let mark = log.length
    await page.clock.fastForward(30_000)
    await settle(page)
    expect(polls(log, mark)).toContain('/api/v1/databases')

    await setHidden(page, true)
    expect(await page.evaluate(() => [document.hidden, document.visibilityState]))
      .toEqual([true, 'hidden'])
    mark = log.length
    for (let spent = 0; spent < 120_000; spent += 10_000) {
      await page.clock.fastForward(10_000)
    }
    await settle(page)
    // Boundary: zero, not "fewer".
    expect(polls(log, mark)).toEqual([])

    // Visible again: each query that missed a tick refetches exactly once.
    mark = log.length
    await setHidden(page, false)
    await settle(page)
    const woke = countBy(polls(log, mark))
    expect(woke['/api/v1/databases']).toBe(1)
    expect(woke['/api/v1/inbox/count']).toBe(1)
    for (const [query, count] of Object.entries(woke)) {
      expect(count, `${query} refetched ${count} times`).toBe(1)
      expect(mounted, `${query} was not polled before the tab hid`).toContain(query)
    }
  })

test('a visible, hidden, visible tab resumes polling at its interval', async ({ page }) => {
  const log = logAPIRequests(page)
  await openActions(page)

  await setHidden(page, true)
  await page.clock.fastForward(45_000)
  await settle(page)
  let mark = log.length
  await setHidden(page, false)
  await settle(page)
  expect(countBy(polls(log, mark))['/api/v1/databases']).toBe(1)

  // The wake fetch restarts the 30 s cadence: nothing 26 s after it, one by
  // 31.5 s (settle lets 0.5 s of real time run on the clock each time).
  mark = log.length
  await page.clock.fastForward(25_000)
  await settle(page)
  expect(polls(log, mark)).not.toContain('/api/v1/databases')
  await page.clock.fastForward(5_000)
  await settle(page)
  expect(countBy(polls(log, mark))['/api/v1/databases']).toBe(1)
})

test('hiding before the first tick falls due sends nothing early on show', async ({ page }) => {
  const log = logAPIRequests(page)
  await openActions(page)

  // Hidden for less than one interval: no poll was skipped, so showing the
  // tab again fetches nothing.
  await setHidden(page, true)
  await page.clock.fastForward(10_000)
  const mark = log.length
  await setHidden(page, false)
  await settle(page)
  expect(polls(log, mark)).toEqual([])
})
