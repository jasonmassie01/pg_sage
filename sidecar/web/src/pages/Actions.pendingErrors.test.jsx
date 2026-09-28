import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// G9-B15: per-database failures must be visible, not "nothing pending".
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
  useAPI: path => ({
    data: path?.includes('/pending')
      ? { pending: [], total: 0,
        errors: [{ database: 'staging', error: 'query failed' }] }
      : { actions: [], total: 0 },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

describe('Pending approval errors', () => {
  it('warns which databases could not be read', () => {
    render(<Actions database="all" user={{ role: 'operator' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-pending'))
    expect(screen.getByTestId('pending-errors'))
      .toHaveTextContent('staging')
  })
})
