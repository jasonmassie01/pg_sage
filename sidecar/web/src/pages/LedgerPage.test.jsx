import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LedgerPage } from './LedgerPage'

// LedgerPage (ux-02a): one row per change from GET /api/v1/outcomes, the
// verdict in words, links to the action and its case. The arithmetic
// (effect percent, measured share) comes from the API; the page formats.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const row = {
  action_id: 12, database: 'orders', action_type: 'create_index', what: 'Add an index',
  why: 'orders lookups read 1.2 GB per call', origin: 'approved',
  expected: { metric: 'blocks_per_call', change_pct: -40, source: 'hypopg' },
  measured: { metric: 'blocks_per_call', change_pct: -42 },
  verdict: 'improved', rollback_state: 'available', evidence_id: 'ev-orders-1',
  finding_id: 9, executed_at: '2026-11-16T03:00:00Z',
}

const unmeasured = {
  action_id: 13, database: 'orders', action_type: 'reindex', what: 'Rebuild an index',
  why: '', origin: 'autonomous', expected: null, measured: null,
  measured_reason: 'suspend epoch break', verdict: 'unverifiable',
  rollback_state: 'not_reversible', executed_at: '2026-11-16T02:00:00Z',
}

const payload = {
  since: '24h', rows: [row, unmeasured], truncated: false, errors: [],
  summary: { changes: 2, verified: 1, reverted: 0, inconclusive: 1, pending: 0,
    measured: 1, measured_share: 0.5 },
}

function response(overrides = {}) {
  return { data: payload, loading: false, error: null, refetch: vi.fn(), ...overrides }
}

describe('LedgerPage', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
    api.useAPI.mockReturnValue(response())
    window.location.hash = '#/ledger'
  })
  afterEach(() => { window.location.hash = '' })

  it('renders one row per change with the verdict as words and links', () => {
    render(<LedgerPage database="all" />)
    const first = screen.getByTestId('ledger-row-orders-12')
    expect(first).toHaveTextContent('Add an index')
    expect(first).toHaveTextContent('orders lookups read 1.2 GB per call')
    expect(first).toHaveTextContent('Improved')
    expect(first).toHaveTextContent('-42% blocks_per_call')
    expect(first).toHaveTextContent('expected -40%')
    expect(first).toHaveTextContent('Can roll back')
    expect(within(first).getByRole('link', { name: /open action 12/i }))
      .toHaveAttribute('href', '#/actions?action=12')
    expect(within(first).getByRole('link', { name: /open case/i }))
      .toHaveAttribute('href', '#/cases?finding=9&database=orders')
    const second = screen.getByTestId('ledger-row-orders-13')
    expect(second).toHaveTextContent('Cannot be verified')
    expect(second).toHaveTextContent('Not measured: suspend epoch break')
    expect(second).toHaveTextContent('Not reversible')
    expect(within(second).queryByRole('link', { name: /open case/i })).toBeNull()
  })

  it('summarizes the window from the API counts', () => {
    render(<LedgerPage database="all" />)
    const summary = screen.getByTestId('ledger-summary')
    expect(summary).toHaveTextContent('2 changes')
    expect(summary).toHaveTextContent('1 verified')
    expect(summary).toHaveTextContent('1 inconclusive')
    expect(summary).toHaveTextContent('1 of 2 with a measured effect')
  })

  it('asks for the default window, the scoped database and a new window', () => {
    render(<LedgerPage database="orders" />)
    expect(api.useAPI.mock.calls[0][0]).toBe('/api/v1/outcomes?since=24h&database=orders')
    fireEvent.change(screen.getByLabelText('Window'), { target: { value: '168h' } })
    const last = api.useAPI.mock.calls[api.useAPI.mock.calls.length - 1][0]
    expect(last).toBe('/api/v1/outcomes?since=168h&database=orders')
  })

  it('filters to one evidence id from the hash over the whole retention', () => {
    window.location.hash = '#/ledger?evidence=ev-xid-7'
    render(<LedgerPage database="all" />)
    expect(api.useAPI.mock.calls[0][0])
      .toBe('/api/v1/outcomes?since=9600h&evidence=ev-xid-7')
    expect(screen.getByTestId('ledger-evidence-filter')).toHaveTextContent('ev-xid-7')
  })

  it('shows an empty window and a database it could not read', () => {
    api.useAPI.mockReturnValue(response({ data: { ...payload, rows: [],
      summary: { changes: 0, verified: 0, reverted: 0, inconclusive: 0, pending: 0,
        measured: 0, measured_share: null },
      errors: [{ database: 'billing', error: 'read outcomes: billing: refused' }] } }))
    render(<LedgerPage database="all" />)
    expect(screen.getByText(/no changes in this window/i)).toBeInTheDocument()
    expect(screen.getByTestId('ledger-summary')).toHaveTextContent('nothing to measure')
    expect(screen.getByRole('alert')).toHaveTextContent('read outcomes: billing: refused')
  })

  it('shows a failed read with a retry', () => {
    const refetch = vi.fn()
    api.useAPI.mockReturnValue(response({ data: null, error: 'HTTP 500', refetch }))
    render(<LedgerPage database="all" />)
    fireEvent.click(screen.getByRole('button', { name: /retry/i }))
    expect(refetch).toHaveBeenCalled()
  })
})
