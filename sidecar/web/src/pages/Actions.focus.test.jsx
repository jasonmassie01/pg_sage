import { act, fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// mcp-05: an Ask Sage citation of action:<id> links to
// #/actions?action=<id>. The page opens the change record's tab and
// shows only that record (the executed row, never a queued proposal
// that shares its number), or says it is not among the loaded records.
// No concurrency tests: the focus is read from the hash on render.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const row = (over) => ({ action_type: 'create_index', outcome: 'success',
  rollback_sql: 'DROP INDEX CONCURRENTLY idx', executed_at: '2026-09-26T09:00:00Z',
  verification_status: 'verified', measured_at: '2026-09-26T09:30:00Z', ...over })
const rows = [
  row({ id: '7', ledger_key: 'queue:7', record_kind: 'queued', status: 'expired',
    outcome: 'expired', sql_executed: 'CREATE INDEX CONCURRENTLY idx ON t_queued(c)' }),
  row({ id: '7', ledger_key: 'log:7', record_kind: 'executed',
    sql_executed: 'CREATE INDEX CONCURRENTLY idx ON t_seven(c)' }),
  row({ id: '8', ledger_key: 'log:8', record_kind: 'executed',
    sql_executed: 'CREATE INDEX CONCURRENTLY idx ON t_eight(c)' }),
]

vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => ({
    data: path?.includes('/pending') ? { pending: [], total: 0 }
      : { actions: rows, total: rows.length },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

const go = hash => window.history.replaceState(null, '', hash)

beforeEach(() => go('#/actions'))

describe('Actions focus from an Ask Sage citation', () => {
  it('opens History with only the cited executed record', () => {
    go('#/actions?action=7&database=orders')
    render(<Actions database="orders" user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('actions-tab-executed')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByTestId('actions-action-focus')).toHaveTextContent('change record 7')
    expect(screen.getAllByText(/t_seven/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/t_eight/)).toBeNull()
    expect(screen.queryByText(/Proposed:/)).toBeNull()
  })

  it('says when the cited record is not loaded', () => {
    go('#/actions?action=999&database=orders')
    render(<Actions database="orders" user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('actions-action-focus'))
      .toHaveTextContent(/change record 999 is not among the loaded/i)
    expect(screen.queryByText(/t_seven/)).toBeNull()
  })

  it('shows every record again from "Show all changes"', () => {
    go('#/actions?action=7&database=orders')
    render(<Actions database="orders" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByRole('link', { name: 'Show all changes' }))
    expect(screen.queryByTestId('actions-action-focus')).toBeNull()
    expect(screen.getAllByText(/t_eight/).length).toBeGreaterThan(0)
  })

  it('follows a hash change to another record', () => {
    go('#/actions?action=7&database=orders')
    render(<Actions database="orders" user={{ role: 'viewer' }} />)
    act(() => {
      go('#/actions?action=8&database=orders')
      window.dispatchEvent(new Event('hashchange'))
    })
    expect(screen.getAllByText(/t_eight/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/t_seven/)).toBeNull()
  })

  it('keeps the default tab without a focus', () => {
    render(<Actions database="orders" user={{ role: 'viewer' }} />)
    expect(screen.queryByTestId('actions-action-focus')).toBeNull()
    expect(screen.getByTestId('actions-tab-recommendations'))
      .toHaveAttribute('aria-selected', 'true')
  })
})
