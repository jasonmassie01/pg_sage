import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Findings } from './Findings'

// The findings list pages with next_cursor and shows a capped total as
// "1000+" (perf v1.8.3, perf-selfexcl).

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const finding = (id, title) => ({ id, title, severity: 'warning', category: 'c',
  subsystem: 'rules', database_name: 'prod', occurrence_count: 1,
  last_seen: '2026-10-03T00:00:00Z', status: 'open' })

let firstPage
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: () => ({ data: firstPage, loading: false, error: null, refetch: vi.fn() }),
}))

afterEach(() => vi.unstubAllGlobals())

describe('Findings paging', () => {
  it('shows a capped total as 1000+', () => {
    firstPage = { findings: [finding(1, 'first')], total: 1000, total_capped: true,
      next_cursor: '' }
    render(<Findings database="all" user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('findings-count')).toHaveTextContent(
      '1000+ total recommendations')
  })

  it('loads the next page with next_cursor and appends it', async () => {
    firstPage = { findings: [finding(1, 'first page row')], total: 2,
      total_capped: false, next_cursor: 'c1' }
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200,
      json: async () => ({ findings: [finding(2, 'second page row')],
        next_cursor: '' }) }))
    vi.stubGlobal('fetch', fetchMock)
    render(<Findings database="all" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('findings-load-more'))
    await waitFor(() => expect(screen.getByText('second page row')).toBeInTheDocument())
    expect(screen.getByText('first page row')).toBeInTheDocument()
    expect(fetchMock.mock.calls[0][0]).toContain('cursor=c1')
    expect(screen.queryByTestId('findings-load-more')).toBeNull()
  })

  it('offers no next page without a cursor', () => {
    firstPage = { findings: [finding(1, 'only')], total: 1, next_cursor: '' }
    render(<Findings database="all" user={{ role: 'viewer' }} />)
    expect(screen.queryByTestId('findings-load-more')).toBeNull()
  })
})
