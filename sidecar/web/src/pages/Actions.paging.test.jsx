import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// The executed actions list pages with next_cursor and shows a capped
// total as "1000+" (perf v1.8.3, perf-selfexcl).

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))
vi.mock('../components/IndexAdmissionPanel', () => ({ IndexAdmissionPanel: () => null }))

const action = (id, sql) => ({ id, action_type: 'create_index', outcome: 'success',
  sql_executed: sql, executed_at: '2026-10-03T00:00:00Z', database_name: 'prod' })

let firstPage
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => (path?.includes('/pending')
    ? { data: { pending: [], total: 0 }, loading: false, error: null, refetch: vi.fn() }
    : { data: firstPage, loading: false, error: null, refetch: vi.fn() }),
}))

afterEach(() => vi.unstubAllGlobals())

describe('Executed actions paging', () => {
  it('shows a capped total as 1000+', () => {
    firstPage = { actions: [action(1, 'CREATE INDEX a ON t (a)')], total: 1000,
      total_capped: true, next_cursor: '' }
    render(<Actions database="all" user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('executed-actions-count')).toHaveTextContent('1000+ actions')
  })

  it('loads the next page with next_cursor and appends it', async () => {
    firstPage = { actions: [action(1, 'CREATE INDEX a ON t (a)')], total: 2,
      total_capped: false, next_cursor: 'c9' }
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200,
      json: async () => ({ actions: [action(2, 'CREATE INDEX b ON t (b)')],
        next_cursor: '' }) }))
    vi.stubGlobal('fetch', fetchMock)
    render(<Actions database="all" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('actions-load-more'))
    await waitFor(() => expect(screen.getAllByText(/CREATE INDEX b ON t/).length)
      .toBeGreaterThan(0))
    expect(fetchMock.mock.calls[0][0]).toContain('cursor=c9')
    expect(screen.getByTestId('executed-actions-count')).toHaveTextContent('2 actions')
    expect(screen.queryByTestId('actions-load-more')).toBeNull()
  })
})
