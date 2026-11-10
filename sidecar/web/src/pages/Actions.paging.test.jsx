import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { stubFetch } from '../test/fetchStub'
import { FakeEventSource } from '../test/fakeEventSource'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { TimeRangeProvider } from '../context/TimeRangeContext'
import { Actions } from './Actions'

// The executed actions list pages with next_cursor and shows a capped
// total as "1000+" (perf v1.8.3, perf-selfexcl). dashboard-01: loaded
// pages survive the 30 s time-range tick, the 30 s poll and live events.
// The real useAPI, TimeRangeProvider and LiveEventsProvider run over
// stubbed fetch and FakeEventSource; only page-unrelated panels are mocked.
// No concurrent access tests: one page, single-threaded.

vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))
vi.mock('../components/IndexAdmissionPanel', () => ({ IndexAdmissionPanel: () => null }))

const action = (id, sql, db = 'prod') => ({ id, ledger_key: `log:${id}`,
  action_type: 'create_index', outcome: 'success', sql_executed: sql,
  executed_at: '2026-10-03T00:00:00Z', database_name: db })
const tick = ms => act(async () => { await vi.advanceTimersByTimeAsync(ms) })
const invoices = () => screen.queryAllByText(/invoices/)

// actionsRoute answers /api/v1/actions: first pages from `firsts` (one per
// call, the last repeats) and the cursor page for cursor=c9.
function actionsRoute(firsts, calls) {
  let n = 0
  return url => {
    calls.push(url)
    if (url.includes('cursor=c9')) {
      return { json: { actions: [action(2, 'CREATE INDEX b ON invoices (b)')],
        next_cursor: '' } }
    }
    const page = firsts[Math.min(n, firsts.length - 1)]
    n += 1
    return { json: page }
  }
}

function renderActions(firsts, calls, database = 'all') {
  stubFetch({
    '/api/v1/actions': actionsRoute(firsts, calls),
    '/api/v1/cases': { json: { cases: [] } },
    '/api/v1/managed-changes': { json: { databases: [] } },
  })
  const view = render(
    <LiveEventsProvider><TimeRangeProvider>
      <Actions database={database} user={{ role: 'viewer' }} />
    </TimeRangeProvider></LiveEventsProvider>)
  return view
}

const first = (extra = {}) => ({ actions: [action(1, 'CREATE INDEX a ON t (a)')],
  total: 2, total_capped: false, next_cursor: 'c9', ...extra })

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-08T14:00:00Z'))
  vi.spyOn(Math, 'random').mockReturnValue(0.5)
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('Executed actions paging', () => {
  it('shows a capped total as 1000+', async () => {
    renderActions([first({ total: 1000, total_capped: true, next_cursor: '' })], [])
    await tick(0)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    expect(screen.getByTestId('executed-actions-count'))
      .toHaveTextContent('1000+ total change records')
  })

  it('loads the next page with next_cursor and appends it', async () => {
    const calls = []
    renderActions([first()], calls)
    await tick(0)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    fireEvent.click(screen.getByTestId('actions-load-more'))
    await tick(0)
    expect(invoices().length).toBeGreaterThan(0)
    expect(calls.at(-1)).toContain('cursor=c9')
    expect(screen.getByTestId('executed-actions-count'))
      .toHaveTextContent('2 total change records')
    expect(screen.queryByTestId('actions-load-more')).toBeNull()
  })

  it('keeps page 2 through the 30 s time-range tick and poll', async () => {
    const calls = []
    renderActions([first()], calls)
    await tick(0)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    fireEvent.click(screen.getByTestId('actions-load-more'))
    await tick(0)
    expect(invoices().length).toBeGreaterThan(0)
    const before = calls.length
    await tick(30000)
    // The tick moved from/to and the poll refetched the first page.
    const refreshes = calls.slice(before).filter(u => !u.includes('cursor='))
    expect(refreshes.length).toBeGreaterThan(0)
    expect(refreshes.at(-1)).toContain('from=2026-11-07T14%3A00%3A30')
    expect(invoices().length).toBeGreaterThan(0)
    await tick(30000)
    expect(invoices().length).toBeGreaterThan(0)
    expect(screen.queryByTestId('actions-load-more')).toBeNull()
  })

  it('keeps page 2 through a live actions event and merges a new head row once', async () => {
    const calls = []
    const fresh = first({ actions: [action(3, 'CREATE INDEX c ON gamma (c)'),
      action(1, 'CREATE INDEX a ON t (a)')], total: 3, next_cursor: 'c1' })
    renderActions([first(), fresh], calls)
    await tick(0)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    fireEvent.click(screen.getByTestId('actions-load-more'))
    await tick(0)
    expect(screen.queryAllByText('Created index on gamma')).toHaveLength(0)
    const source = FakeEventSource.instances.at(-1)
    act(() => source.open())
    act(() => source.emit('actions', { type: 'actions' }))
    await tick(2000) // live refetches wait out a 2 s debounce (dashboard-08)
    // Each row shows its summary once: the merge adds the new head row and
    // duplicates nothing.
    expect(screen.getAllByText('Created index on gamma')).toHaveLength(1)
    expect(screen.getAllByText('Created index on t')).toHaveLength(1)
    expect(screen.getAllByText('Created index on invoices')).toHaveLength(1)
    expect(screen.getByTestId('executed-actions-count'))
      .toHaveTextContent('3 loaded history records')
  })

  it('resets page 2 on a database switch', async () => {
    const calls = []
    const { rerender } = renderActions([first()], calls, 'prod')
    await tick(0)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    fireEvent.click(screen.getByTestId('actions-load-more'))
    await tick(0)
    expect(invoices().length).toBeGreaterThan(0)
    rerender(<LiveEventsProvider><TimeRangeProvider>
      <Actions database="staging" user={{ role: 'viewer' }} />
    </TimeRangeProvider></LiveEventsProvider>)
    await tick(0)
    expect(calls.some(u => u.includes('database=staging'))).toBe(true)
    expect(invoices()).toHaveLength(0)
  })
})
