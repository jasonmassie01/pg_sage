import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// G9-B01 / G6-B05 / G9-B04 / G9-B14 / SURF-12: the merged ledger must
// distinguish queued proposals from executed actions.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const queuedRow = {
  id: '7', ledger_key: 'queue:7', record_kind: 'queued',
  action_type: 'create_index', status: 'pending', outcome: 'pending',
  sql_executed: 'CREATE INDEX CONCURRENTLY idx_q ON t(c)',
  rollback_sql: 'DROP INDEX CONCURRENTLY idx_q',
  executed_at: '2026-09-26T10:00:00Z', verification_status: 'not_started',
}
const executedRow = {
  id: '7', ledger_key: 'log:7', record_kind: 'executed',
  action_type: 'create_index', outcome: 'success',
  sql_executed: 'CREATE INDEX CONCURRENTLY idx_l ON t(c)',
  rollback_sql: 'DROP INDEX CONCURRENTLY idx_l',
  executed_at: '2026-09-26T09:00:00Z', verification_status: 'verified',
  measured_at: '2026-09-26T09:30:00Z',
}

vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => ({
    data: path?.includes('/pending')
      ? { pending: [], total: 0 }
      : { actions: [queuedRow, executedRow], total: 2 },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

afterEach(() => vi.unstubAllGlobals())

function rowFor(text) {
  return screen.getAllByRole('row').find(r => r.textContent.includes(text))
}

describe('Action ledger record kinds', () => {
  it('never offers rollback on a queued proposal', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)
    const queued = screen.getAllByLabelText('Expand row')[0]
    fireEvent.click(queued)
    expect(screen.queryByTestId('rollback-action-button')).toBeNull()
    expect(screen.getByText('Proposed SQL')).toBeInTheDocument()
    expect(screen.queryByText('SQL Executed')).toBeNull()
  })

  it('labels queued proposals as awaiting approval, not monitoring', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)
    expect(screen.queryByText('Monitoring')).toBeNull()
    expect(screen.getByText('Pending approval')).toBeInTheDocument()
  })

  it('offers rollback on an executed action and sends record_kind', async () => {
    const fetch = vi.fn().mockResolvedValue({
      ok: true, json: async () => ({ ok: true }),
    })
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('confirm', () => true)
    render(<Actions database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getAllByLabelText('Expand row')[1])
    const buttons = screen.getAllByTestId('rollback-action-button')
    expect(buttons).toHaveLength(1)
    expect(screen.getByText('SQL Executed')).toBeInTheDocument()
    fireEvent.click(buttons[0])
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/actions/7/rollback')
    expect(JSON.parse(init.body).record_kind).toBe('executed')
  })

  it('expands exactly one row when queue and log ids collide', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getAllByLabelText('Expand row')[0])
    expect(screen.getAllByLabelText('Collapse row')).toHaveLength(1)
    expect(screen.getAllByLabelText('Expand row')).toHaveLength(1)
  })

  it('shows the server verification status for executed rows', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)
    const executed = rowFor('idx_l') || screen.getAllByRole('row')[2]
    expect(within(executed).getByText('verified')).toBeInTheDocument()
  })
})
