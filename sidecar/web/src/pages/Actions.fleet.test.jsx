import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: () => ({
    data: { actions: [], pending: ['first', 'second'].map(database_name => ({
      id: 7, finding_id: 9, database_name, action_risk: 'safe',
      proposed_sql: `ANALYZE public.${database_name}`, status: 'pending',
    })) },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

afterEach(() => vi.unstubAllGlobals())

describe('Fleet action identity', () => {
  it('approves the selected database when local action IDs collide', async () => {
    const fetch = vi.fn().mockResolvedValue({ json: async () => ({ ok: true }) })
    vi.stubGlobal('fetch', fetch)
    render(<Actions database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-pending'))
    fireEvent.click(screen.getAllByTestId('approve-button')[1])
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/actions/7/approve?database=second', expect.objectContaining({ method: 'POST' }),
    ))
  })

  it('expands and rejects only the selected database row', async () => {
    const fetch = vi.fn().mockResolvedValue({ json: async () => ({ ok: true }) })
    vi.stubGlobal('fetch', fetch)
    render(<Actions database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-pending'))
    fireEvent.click(screen.getAllByLabelText('Expand row')[1])
    expect(screen.getAllByLabelText('Collapse row')).toHaveLength(1)
    fireEvent.click(screen.getByLabelText('Collapse row'))
    const second = screen.getByRole('cell', { name: 'second', exact: true }).closest('tr')
    fireEvent.click(within(second).getByTestId('reject-button'))
    expect(screen.getAllByPlaceholderText('Reason for rejection...')).toHaveLength(1)
    fireEvent.change(screen.getByPlaceholderText('Reason for rejection...'), {
      target: { value: 'Keep the selected database unchanged' },
    })
    fireEvent.click(screen.getByText('Confirm Reject'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/actions/7/reject?database=second', expect.objectContaining({ method: 'POST' }),
    ))
  })
})
