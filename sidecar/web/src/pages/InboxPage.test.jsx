import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { InboxPage } from './InboxPage'

// ux-01b: the Inbox is the home page. (a) Needs you: ux-01a's items, each
// decided in place through its own route with X-Sage-Surface: inbox;
// (b) Since you were last here: ux-02a's ledger rows since inbox_seen_at,
// with Undo where a rollback is available; (c) one Value line.
// No concurrent access tests here: one render tree; two tabs marking the
// seen time at once is api.TestInboxSeenDB_ConcurrentMarksKeepTheLaterTime.

const api = vi.hoisted(() => ({ useAPI: vi.fn(), toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock('../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('../components/Toast', () => ({ useToast: () => api.toast }))

const operator = { email: 'op@example.com', role: 'operator' }
const SEEN = '2026-11-10T15:00:00Z' // a Tuesday
const NOW = new Date('2026-11-12T18:00:00Z') // the Thursday after

const item = (kind, id, extra = {}) => ({ kind, id: String(id), database: 'orders',
  title: `${kind} ${id}`, why: `why ${id}`, risk: 'moderate', created_at: SEEN,
  href: `#/somewhere?id=${id}`, source: 'x', actionable: true, ...extra })
const threeItems = [
  item('change_approval', 42, { title: 'Add index on orders(customer_id)', risk: 'high',
    href: '#/actions?tab=pending&queue=42&database=orders' }),
  item('fact', 7, { title: 'idx_legacy is owned by the billing app', risk: 'safe' }),
  item('autonomy_promotion', 'p-3', { title: 'Let pg_sage create indexes on its own' }),
]
const change = (id, extra = {}) => ({ action_id: id, database: 'orders',
  action_type: 'create_index', what: `change ${id}`, why: '', origin: 'approved',
  verdict: 'improved', rollback_state: 'not_reversible', executed_at: SEEN, ...extra })
const card = { queue_id: 42, database: 'orders', title: 'Add index on orders(customer_id)',
  card_hash: 'h42', sql: 'CREATE INDEX CONCURRENTLY idx_c ON orders (customer_id)',
  expires_at: '2026-11-20T00:00:00Z', risk: { tier: 'moderate' } }

let queries
function q(data, extra = {}) {
  return { data, loading: false, error: null, refetch: vi.fn(), updatedAt: 1, ...extra }
}
function setAPI({ items = [], errors = [], rows = [], value = null, inbox } = {}) {
  queries = {
    inbox: inbox || q({ items, errors, total: items.length, lower_bound: errors.length > 0,
      counts_by_kind: {}, truncated: false }),
    outcomes: q({ rows, errors: [], summary: {}, since: '48h', truncated: false }),
    value: q(value),
    card: q({ card, eligible: true }),
  }
  api.useAPI.mockImplementation(raw => {
    const url = typeof raw === 'function' ? raw() : raw
    if (!url) return { data: null, loading: false, error: null, refetch: vi.fn() }
    if (url.startsWith('/api/v1/inbox')) return queries.inbox
    if (url.startsWith('/api/v1/outcomes')) return queries.outcomes
    if (url.startsWith('/api/v1/value')) return queries.value
    if (url.startsWith('/api/v1/approvals/')) return queries.card
    throw new Error(`unexpected useAPI ${url}`)
  })
}

function jsonResponse(status, body) {
  return { ok: status >= 200 && status < 300, status, statusText: '',
    json: async () => body, text: async () => JSON.stringify(body) }
}
let posts
function routeFetch({ seen = { seen_at: SEEN }, seenStatus = 200, action = {} } = {}) {
  posts = []
  const fetch = vi.fn(async (url, init = {}) => {
    if (init.method === 'POST') {
      posts.push({ url, headers: init.headers, body: init.body ? JSON.parse(init.body) : null })
      if (url.includes('/rollback')) return jsonResponse(200, { ok: true, status: 'rolled_back' })
      return jsonResponse(200, { ok: true, status: 'executed', executed: true })
    }
    if (url === '/api/v1/inbox/seen') return jsonResponse(seenStatus, seen)
    if (url.startsWith('/api/v1/actions/')) {
      return jsonResponse(200, { id: 12, database_name: 'orders', record_kind: 'executed',
        rollback_sql: 'DROP INDEX CONCURRENTLY IF EXISTS idx_c', ...action })
    }
    throw new Error(`unexpected fetch ${url}`)
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

async function renderInbox(props = {}) {
  let view
  await act(async () => {
    view = render(<InboxPage database="orders" user={operator} {...props} />)
  })
  return view
}
const outcomesURL = () => api.useAPI.mock.calls.map(([u]) => (typeof u === 'function' ? u() : u))
  .filter(u => u?.startsWith('/api/v1/outcomes')).at(-1)

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(NOW)
  api.useAPI.mockReset()
  api.toast.success.mockReset()
  api.toast.error.mockReset()
  setAPI()
  routeFetch()
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('InboxPage needs you', () => {
  it('renders three pending items of three kinds', async () => {
    setAPI({ items: threeItems })
    await renderInbox()
    const list = screen.getByTestId('inbox-needs-you')
    expect(within(list).getAllByTestId(/^inbox-item-/)).toHaveLength(3)
    expect(within(list).getByTestId('inbox-item-change_approval-42'))
      .toHaveTextContent('Add index on orders(customer_id)')
    expect(within(list).getByTestId('inbox-item-change_approval-42')).toHaveTextContent('Change approval')
    expect(within(list).getByTestId('inbox-item-fact-7')).toHaveTextContent('Fact')
    expect(within(list).getByTestId('inbox-item-autonomy_promotion-p-3'))
      .toHaveTextContent('Autonomy promotion')
    expect(within(list).getByTestId('inbox-item-change_approval-42')).toHaveTextContent('High')
    expect(within(screen.getByTestId('inbox-item-change_approval-42'))
      .getByRole('link', { name: /open/i }))
      .toHaveAttribute('href', '#/actions?tab=pending&queue=42&database=orders')
  })

  it('approves the change approval in place with today\'s body plus the header', async () => {
    setAPI({ items: threeItems })
    const view = await renderInbox()
    fireEvent.click(within(screen.getByTestId('inbox-item-change_approval-42'))
      .getByRole('button', { name: /review/i }))
    expect(api.useAPI.mock.calls.some(([u]) => u === '/api/v1/approvals/42?database=orders'))
      .toBe(true)
    await act(async () => { fireEvent.click(screen.getByTestId('approval-approve')) })
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0].url).toBe('/api/v1/approvals/42/approve?database=orders')
    expect(posts[0].body).toEqual({ card_hash: 'h42' })
    expect(posts[0].headers).toEqual({ 'Content-Type': 'application/json',
      'X-Sage-Surface': 'inbox' })
    expect(queries.inbox.refetch).toHaveBeenCalled()
    // The next poll no longer lists it: it leaves (a).
    setAPI({ items: threeItems.slice(1) })
    view.rerender(<InboxPage database="orders" user={operator} />)
    expect(screen.queryByTestId('inbox-item-change_approval-42')).toBeNull()
    expect(screen.getAllByTestId(/^inbox-item-/)).toHaveLength(2)
  })

  it('decides a fact in place with the header', async () => {
    setAPI({ items: threeItems })
    await renderInbox()
    await act(async () => {
      fireEvent.click(within(screen.getByTestId('inbox-item-fact-7'))
        .getByRole('button', { name: 'Confirm' }))
    })
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0].url).toBe('/api/v1/facts/7/confirm?database=orders')
    expect(posts[0].headers['X-Sage-Surface']).toBe('inbox')
    expect(queries.inbox.refetch).toHaveBeenCalled()
  })

  it('renders a hostile title as text', async () => {
    setAPI({ items: [item('fact', 1, { title: '<img src=x onerror="alert(1)">' })] })
    const { container } = await renderInbox()
    expect(screen.getByTestId('inbox-item-fact-1'))
      .toHaveTextContent('<img src=x onerror="alert(1)">')
    expect(container.querySelector('img')).toBeNull()
  })

  it('offers a viewer no decision, only the link', async () => {
    setAPI({ items: threeItems.map(i => ({ ...i, actionable: false })) })
    await renderInbox({ user: { email: 'v@example.com', role: 'viewer' } })
    expect(screen.queryByRole('button', { name: /review|confirm|approve/i })).toBeNull()
    expect(screen.getAllByText(/an operator or admin decides this/i)).toHaveLength(2)
    // A promotion is an admin's decision (ux-01a CanDecide).
    expect(screen.getByTestId('inbox-item-autonomy_promotion-p-3'))
      .toHaveTextContent(/an admin decides this/i)
  })

  it('shows the error with Retry on a 500, never the goal state', async () => {
    setAPI({ inbox: q(null, { error: 'internal error (500)', errorStatus: 500, updatedAt: null }) })
    await renderInbox()
    expect(screen.getByText(/internal error \(500\)/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
    expect(screen.queryByText(/Nothing needs you/)).toBeNull()
  })

  it('counts every waiting item in the heading when the list is cut short', async () => {
    setAPI({ inbox: q({ items: threeItems, errors: [], total: 30, lower_bound: false,
      counts_by_kind: {}, truncated: true }) })
    await renderInbox()
    expect(screen.getByRole('heading', { name: 'Needs you (30)' })).toBeInTheDocument()
    expect(screen.getByTestId('inbox-truncated'))
      .toHaveTextContent('Showing the first 3 of 30; open each page for the rest.')
  })

  it('names a failed source above the list and claims no goal state', async () => {
    setAPI({ errors: [{ source: 'autonomy_promotion', database: 'orders',
      error: 'timed out after 2s' }] })
    await renderInbox()
    expect(screen.getByTestId('inbox-source-errors'))
      .toHaveTextContent('Could not read: autonomy promotions (orders)')
    expect(screen.queryByText(/Nothing needs you/)).toBeNull()
    expect(screen.getByText(/Nothing waits in the sources that answered/)).toBeInTheDocument()
  })
})

describe('InboxPage since you were last here', () => {
  it('is the goal state with no count sentence when nothing happened', async () => {
    await renderInbox()
    expect(screen.getByTestId('inbox-goal')).toHaveTextContent(/^Nothing needs you\.$/)
  })

  it('words the goal state from the counts', async () => {
    setAPI({ rows: [1, 2, 3, 4, 5, 6].map(id => change(id)) })
    const view = await renderInbox()
    expect(screen.getByTestId('inbox-goal'))
      .toHaveTextContent('Nothing needs you. 6 changes since Tuesday, all verified.')
    setAPI({ rows: [change(1), change(2, { verdict: 'pending' }), change(3)] })
    view.rerender(<InboxPage database="orders" user={operator} />)
    expect(screen.getByTestId('inbox-goal'))
      .toHaveTextContent('Nothing needs you. 3 changes since Tuesday, 2 verified.')
  })

  it('claims no full count while a database ledger could not be read', async () => {
    setAPI({ rows: [change(1), change(2)] })
    queries.outcomes = q({ rows: [change(1), change(2)], summary: {}, since: '48h',
      truncated: false, errors: [{ database: 'billing', error: 'timed out' }] })
    await renderInbox()
    expect(screen.getByTestId('inbox-goal'))
      .toHaveTextContent(/^Nothing needs you\. At least 2 changes since Tuesday, 2 verified\.$/)
    expect(screen.getByTestId('inbox-ledger-partial'))
      .toHaveTextContent('Could not read the ledger of: billing (timed out)')
  })

  it('says at least when the ledger page is full of changes since the seen time', async () => {
    const rows = Array.from({ length: 500 }, (_, i) => change(i + 1))
    setAPI({ rows })
    await renderInbox()
    expect(screen.getByTestId('inbox-goal'))
      .toHaveTextContent('Nothing needs you. At least 500 changes since Tuesday, 500 verified.')
    expect(screen.getByTestId('inbox-ledger-partial'))
      .toHaveTextContent('Showing the newest 500 changes; the Ledger page has the rest.')
  })

  it('counts exactly when the full page reaches back past the seen time', async () => {
    const rows = Array.from({ length: 499 }, (_, i) => change(i + 1))
    rows.push(change(999, { executed_at: '2026-11-09T00:00:00Z' }))
    setAPI({ rows })
    await renderInbox()
    expect(screen.getByTestId('inbox-goal'))
      .toHaveTextContent('Nothing needs you. 499 changes since Tuesday, all verified.')
    expect(screen.queryByTestId('inbox-ledger-partial')).toBeNull()
  })

  it('reads the ledger since the seen time, a change exactly at it once', async () => {
    setAPI({ rows: [change(1), change(1), change(2, { executed_at: '2026-11-10T14:59:59Z' })] })
    await renderInbox()
    // 2026-11-12T18:00Z - 2026-11-10T15:00Z = 183600 s, plus an hour of slack
    // (the window is fixed when the page opens; rows are cut at the seen time).
    expect(outcomesURL()).toBe('/api/v1/outcomes?since=187200s&database=orders')
    const rows = screen.getAllByTestId(/^inbox-change-/)
    expect(rows).toHaveLength(1)
    expect(rows[0]).toHaveAttribute('data-testid', 'inbox-change-orders-1')
  })

  it('shows the last 24 hours on a first visit, labelled so', async () => {
    routeFetch({ seen: { seen_at: null } })
    await renderInbox()
    expect(outcomesURL()).toBe('/api/v1/outcomes?since=24h&database=orders')
    expect(screen.getByTestId('inbox-since-label'))
      .toHaveTextContent('First visit: changes in the last 24 hours')
  })

  it('falls back to the last 24 hours, saying why, when the seen time cannot be read', async () => {
    routeFetch({ seenStatus: 500, seen: { error: 'could not read the inbox seen time' } })
    await renderInbox()
    expect(outcomesURL()).toBe('/api/v1/outcomes?since=24h&database=orders')
    expect(screen.getByTestId('inbox-since-label'))
      .toHaveTextContent(/could not read when you were last here/i)
  })

  it('undoes a change with a reason and shows it rolled back', async () => {
    setAPI({ rows: [change(12, { rollback_state: 'available' })] })
    const view = await renderInbox()
    await act(async () => {
      fireEvent.click(within(screen.getByTestId('inbox-change-orders-12'))
        .getByRole('button', { name: 'Undo' }))
    })
    const dialog = await screen.findByRole('dialog', { name: /Roll back action 12/ })
    expect(within(dialog).getByTestId('confirm-dialog-sql'))
      .toHaveTextContent('DROP INDEX CONCURRENTLY IF EXISTS idx_c')
    fireEvent.change(within(dialog).getByTestId('confirm-dialog-reason'),
      { target: { value: 'checkout writes slowed down' } })
    await act(async () => { fireEvent.click(within(dialog).getByTestId('confirm-dialog-confirm')) })
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0]).toEqual({ url: '/api/v1/actions/12/rollback?database=orders',
      headers: { 'Content-Type': 'application/json', 'X-Sage-Surface': 'inbox' },
      body: { reason: 'checkout writes slowed down', record_kind: 'executed' } })
    expect(queries.outcomes.refetch).toHaveBeenCalled()
    setAPI({ rows: [change(12, { rollback_state: 'rolled_back' })] })
    view.rerender(<InboxPage database="orders" user={operator} />)
    const row = screen.getByTestId('inbox-change-orders-12')
    expect(row).toHaveTextContent('Rolled back: checkout writes slowed down')
    expect(within(row).queryByRole('button', { name: 'Undo' })).toBeNull()
  })

  it('offers no Undo to a viewer or on a row that cannot roll back', async () => {
    setAPI({ rows: [change(12, { rollback_state: 'available' }), change(13)] })
    const view = await renderInbox({ user: { email: 'v@example.com', role: 'viewer' } })
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull()
    view.rerender(<InboxPage database="orders" user={operator} />)
    expect(within(screen.getByTestId('inbox-change-orders-13'))
      .queryByRole('button', { name: 'Undo' })).toBeNull()
  })
})

describe('InboxPage seen time and value', () => {
  it('marks the page seen after 5 s visible, once, with the time it opened', async () => {
    vi.useRealTimers() // re-fake: beforeEach faked Date only
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] })
    vi.setSystemTime(NOW)
    await renderInbox()
    await act(async () => { vi.advanceTimersByTime(4999) })
    expect(posts).toEqual([])
    vi.setSystemTime(new Date(NOW.getTime() + 5000))
    await act(async () => { vi.advanceTimersByTime(1) })
    expect(posts).toHaveLength(1)
    expect(posts[0].url).toBe('/api/v1/inbox/seen')
    expect(posts[0].body).toEqual({ seen_at: NOW.toISOString() })
    await act(async () => { vi.advanceTimersByTime(60000) })
    expect(posts).toHaveLength(1)
  })

  it('does not mark a hidden page seen', async () => {
    vi.useRealTimers() // re-fake: beforeEach faked Date only
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] })
    vi.setSystemTime(NOW)
    const spy = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    await renderInbox()
    await act(async () => { vi.advanceTimersByTime(10000) })
    expect(posts).toEqual([])
    spy.mockRestore()
  })

  it('shows one Value line from /api/v1/value', async () => {
    setAPI({ value: { dba_hours_saved: { all_time: 40, this_week: 3.5 } } })
    await renderInbox()
    const line = screen.getByTestId('inbox-value')
    expect(line).toHaveTextContent('Verified DBA hours saved: 3.5 h this week, 40 h all time.')
    expect(within(line).getByRole('link')).toHaveAttribute('href', '#/value')
  })
})
