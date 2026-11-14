import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// dashboard-31: pins what the executed and pending lists show, and how the
// table-view decisions behave, so splitting Actions.jsx changes nothing.
// The useAPI fake reads a shared store and re-renders on refetch, so a
// decision followed by a refetch shows the server's next state.

const h = vi.hoisted(() => ({
  store: { pending: [], actions: [], pendingErrors: [] },
  toast: { success: () => {}, error: () => {} },
}))

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => () => {} }))
vi.mock('../components/Toast', () => ({ useToast: () => h.toast }))
vi.mock('../hooks/useAPI', async () => {
  const { useReducer } = await import('react')
  function respond(path) {
    if (path?.includes('/api/v1/actions/pending')) {
      return { pending: h.store.pending, total: h.store.pending.length,
        errors: h.store.pendingErrors }
    }
    if (path?.startsWith('/api/v1/approvals/')) return { card: null, eligible: true }
    if (path?.startsWith('/api/v1/approvals')) return { actions: [] }
    return { actions: h.store.actions, total: h.store.actions.length }
  }
  return {
    withTimeRange: path => path,
    useAPI: path => {
      const [, bump] = useReducer(n => n + 1, 0)
      return { data: path ? respond(path) : null, loading: false, error: null,
        refetch: bump }
    },
  }
})

const executedRows = [
  { id: '1', ledger_key: 'log:1', record_kind: 'executed', database_name: 'orders',
    action_type: 'create_index', outcome: 'success', action_risk: 'safe',
    sql_executed: 'CREATE INDEX CONCURRENTLY idx_a ON public.orders(id)',
    executed_at: '2026-10-03T10:00:00Z', verification_outcome: { verdict: 'improved' } },
  { id: '2', ledger_key: 'log:2', record_kind: 'executed', database_name: 'orders',
    action_type: 'drop_index', outcome: 'failed', action_risk: 'high',
    rollback_reason: 'lock timeout on idx_b', sql_executed: 'DROP INDEX CONCURRENTLY idx_b;',
    executed_at: '2026-10-03T09:00:00Z' },
  { id: '3', ledger_key: 'log:3', record_kind: 'executed', database_name: 'events',
    action_type: 'vacuum', outcome: 'rolled_back', action_risk: 'moderate', attempts: 2,
    sql_executed: 'VACUUM public.events;', executed_at: '2026-10-03T08:00:00Z' },
]

const longSQL = 'CREATE INDEX CONCURRENTLY idx_events_tenant_created_at ON public.events ' +
  '(tenant_id, created_at) WHERE deleted_at IS NULL'

const pendingRows = [
  { id: 41, database_name: 'orders', action_type: 'analyze_table', action_risk: 'safe',
    finding_id: 12, status: 'pending', proposed_sql: 'ANALYZE public.orders',
    proposed_at: '2026-10-03T11:00:00Z' },
  { id: 42, database_name: 'orders', action_type: 'create_index', action_risk: 'high',
    finding_id: 13, status: 'pending', eligible: false, defer_reason: 'maintenance_window',
    policy_detail: { window_only: true },
    proposed_sql: 'CREATE INDEX CONCURRENTLY idx_c ON public.orders(c)',
    proposed_at: '2026-10-03T11:05:00Z' },
  { id: 43, database_name: 'events', action_type: 'create_index', action_risk: 'moderate',
    finding_id: 14, status: 'pending', eligible: false,
    blocked_reason: 'insufficient_privilege', lifecycle_state: 'blocked',
    proposed_sql: longSQL, proposed_at: '2026-10-03T11:10:00Z' },
]

beforeEach(() => {
  h.store.pending = pendingRows.map(r => ({ ...r }))
  h.store.actions = executedRows.map(r => ({ ...r }))
  h.store.pendingErrors = []
  h.toast = { success: vi.fn(), error: vi.fn() }
  window.location.hash = ''
})
afterEach(() => vi.unstubAllGlobals())

function rowFor(text) {
  return screen.getAllByRole('row').find(r => r.textContent.includes(text))
}

function openTab(tab, role = 'operator') {
  render(<Actions database="all" user={{ role }} />)
  fireEvent.click(screen.getByTestId(`actions-tab-${tab}`))
}

function deferred() {
  let resolve
  const promise = new Promise(r => { resolve = r })
  return { promise, resolve }
}

function jsonResponse(status, body) {
  return { status, ok: status >= 200 && status < 300, json: async () => body }
}

describe('Actions executed list rows', () => {
  it.each([
    ['Created index on orders', ['Create index', 'Success', 'Low', 'Improved', 'orders']],
    ['lock timeout on idx_b', ['Drop index', 'Failed', 'High', 'not_started', 'orders']],
    ['Vacuumed events', ['Vacuum', 'Rolled Back', 'Moderate', '2×', 'events']],
  ])('shows %s with its type, outcome, risk, verification and database', (summary, parts) => {
    openTab('executed', 'admin')
    const row = rowFor(summary)
    expect(row).toBeTruthy()
    for (const part of parts) expect(row).toHaveTextContent(part)
  })

  it('shows the column set the rows need and the loaded count', () => {
    openTab('executed', 'admin')
    const table = screen.getByTestId('executed-actions-table')
    for (const label of ['Type', 'Summary', 'Outcome', 'Risk', 'Attempts', 'Verification',
      'Database', 'When']) {
      expect(within(table).getByRole('columnheader', { name: label })).toBeInTheDocument()
    }
    expect(screen.getByTestId('executed-actions-count'))
      .toHaveTextContent('3 loaded history records')
    expect(screen.getByTestId('actions-tab-executed')).toHaveTextContent('(3)')
  })

  it('shows the error details and executed SQL of a failed row', () => {
    openTab('executed', 'admin')
    fireEvent.click(within(rowFor('lock timeout on idx_b')).getByLabelText('Expand row'))
    expect(screen.getByText('Error Details').parentElement)
      .toHaveTextContent('lock timeout on idx_b')
    // SQLBlock highlights tokens, so the SQL is read from its section text.
    expect(screen.getByText('SQL Executed').parentElement)
      .toHaveTextContent('DROP INDEX CONCURRENTLY idx_b;')
  })

  it('shows the empty state when no row is loaded', () => {
    h.store.actions = []
    openTab('executed', 'admin')
    expect(screen.queryByTestId('executed-actions-table')).toBeNull()
    expect(screen.getByText('No completed change records are loaded for the selected time range.'))
      .toBeInTheDocument()
  })
})

describe('Actions pending list rows', () => {
  it.each([
    [12, ['Low Risk', 'orders', 'ANALYZE public.orders',
      'Awaiting your decision; safety checks run again before execution.', 'Approve and run']],
    [13, ['High Risk', 'orders', 'Waiting for an allowed maintenance window.',
      'Approve for the next window']],
    [14, ['Moderate Risk', 'events', 'blocked',
      'This connection lacks permission for the proposed change.', longSQL.slice(0, 90) + '...']],
  ])('shows finding %s with its risk, database, availability and decision', (finding, parts) => {
    openTab('pending')
    const row = screen.getAllByRole('row').find(r => within(r).queryByText(String(finding)))
    expect(row).toBeTruthy()
    for (const part of parts) expect(row).toHaveTextContent(part)
  })

  it('enables approve only where policy allows it', () => {
    openTab('pending')
    const buttons = screen.getAllByTestId('approve-button')
    expect(buttons.map(b => b.disabled)).toEqual([false, false, true])
    expect(screen.getByTestId('actions-tab-pending')).toHaveTextContent('(3)')
  })
})

describe('Actions pending decisions', () => {
  it('posts the approve to the row database and shows a 409 without dropping the row', async () => {
    h.store.pendingErrors = [{ database: 'staging', error: 'query failed' }]
    const fetchMock = vi.fn(async () => jsonResponse(409, { code: 'content_changed' }))
    vi.stubGlobal('fetch', fetchMock)
    openTab('pending')
    fireEvent.click(screen.getAllByTestId('approve-button')[0])
    await screen.findByText(/This action changed after this card was shown/)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/actions/41/approve?database=orders')
    expect(init).toEqual({ method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' } })
    expect(h.toast.error).toHaveBeenCalledWith(expect.stringContaining('changed after'))
    expect(h.toast.success).not.toHaveBeenCalled()
    expect(screen.getByTestId('pending-errors')).toHaveTextContent('staging (query failed)')
    expect(screen.getAllByTestId('approve-button')).toHaveLength(3)
    expect(rowFor('ANALYZE public.orders')).toBeTruthy()
  })

  it('reports a network failure of approve and keeps the row', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('network down') }))
    openTab('pending')
    fireEvent.click(screen.getAllByTestId('approve-button')[0])
    await screen.findByText('network down')
    expect(h.toast.error).toHaveBeenCalledWith('Approve failed: network down')
    expect(screen.getAllByTestId('approve-button')).toHaveLength(3)
  })

  it('moves an approved row from pending to history after the refetch', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => {
      h.store.pending = h.store.pending.filter(r => r.id !== 41)
      h.store.actions = [{ id: '90', ledger_key: 'log:90', record_kind: 'executed',
        database_name: 'orders', action_type: 'analyze', outcome: 'success',
        sql_executed: 'ANALYZE public.orders;', verification_outcome: { verdict: 'neutral' },
        executed_at: '2026-10-03T12:00:00Z' }, ...h.store.actions]
      return jsonResponse(200, { ok: true, executed: true, verification_status: 'pending' })
    }))
    openTab('pending')
    fireEvent.click(screen.getAllByTestId('approve-button')[0])
    await screen.findByText('Approved and executed; verification: pending')
    expect(h.toast.success).toHaveBeenCalledWith('Approved and executed; verification: pending')
    expect(screen.getAllByTestId('approve-button')).toHaveLength(2)
    expect(rowFor('ANALYZE public.orders')).toBeUndefined()
    expect(screen.getByTestId('actions-tab-pending')).toHaveTextContent('(2)')
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    expect(rowFor('Updated statistics for orders')).toHaveTextContent('Neutral')
    expect(screen.getByTestId('executed-actions-count'))
      .toHaveTextContent('4 loaded history records')
  })

  it('rejects with the typed reason and removes the row', async () => {
    const fetchMock = vi.fn(async () => {
      h.store.pending = h.store.pending.filter(r => r.id !== 42)
      return jsonResponse(200, { ok: true })
    })
    vi.stubGlobal('fetch', fetchMock)
    openTab('pending')
    fireEvent.click(screen.getAllByTestId('reject-button')[1])
    const confirm = screen.getByRole('button', { name: 'Confirm Reject' })
    expect(confirm).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Reason for rejection'),
      { target: { value: '  not during peak  ' } })
    fireEvent.click(confirm)
    await screen.findByText('Action 42 rejected')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/actions/42/reject?database=orders')
    expect(init).toEqual({ method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason: 'not during peak' }) })
    expect(screen.getAllByTestId('reject-button')).toHaveLength(2)
    expect(screen.queryByLabelText('Reason for rejection')).toBeNull()
    expect(screen.getByTestId('actions-tab-pending')).toHaveTextContent('(2)')
  })

  it('shows a refused reject and keeps the row', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse(403, { error: 'not allowed' })))
    openTab('pending')
    fireEvent.click(screen.getAllByTestId('reject-button')[0])
    fireEvent.change(screen.getByLabelText('Reason for rejection'), { target: { value: 'no' } })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm Reject' }))
    await screen.findByText('not allowed')
    expect(h.toast.error).toHaveBeenCalledWith('not allowed')
    expect(screen.getAllByTestId('reject-button')).toHaveLength(3)
  })

  it('approves different rows concurrently, one request each', async () => {
    const replies = [deferred(), deferred()]
    const fetchMock = vi.fn(() => replies[fetchMock.mock.calls.length - 1].promise)
    vi.stubGlobal('fetch', fetchMock)
    openTab('pending')
    const [first, second] = screen.getAllByTestId('approve-button')
    fireEvent.click(first)
    fireEvent.click(second)
    expect(fetchMock.mock.calls.map(c => c[0])).toEqual([
      '/api/v1/actions/41/approve?database=orders',
      '/api/v1/actions/42/approve?database=orders',
    ])
    await act(async () => {
      for (const r of replies) r.resolve(jsonResponse(200, { ok: true, executed: true }))
    })
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})
