import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// ux-03: Plans is the first tab of Actions; a link with tab=plans opens it.
// Proposed changes stays the default tab (the plan names no new default).

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const requested = []
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => {
    requested.push(path)
    const plans = { plans: [{ database: 'orders', id: 7, goal: 'cut blocks per call',
      status: 'proposed', created_at: '2026-11-16T09:30:00Z' }], total: 1, errors: [] }
    return { data: path?.startsWith('/api/v1/plans') ? plans
      : { actions: [], pending: [], total: 0 }, loading: false, error: null,
    refetch: vi.fn() }
  },
}))

afterEach(() => {
  window.location.hash = ''
  requested.length = 0
})

describe('Actions > Plans', () => {
  it('puts Plans first in the tab bar, for viewers too', () => {
    for (const role of ['operator', 'viewer']) {
      const { unmount } = render(<Actions database="all" user={{ role }} />)
      const tabs = screen.getAllByRole('tab').map(t => t.dataset.testid)
      expect(tabs[0], role).toBe('actions-tab-plans')
      expect(screen.getByTestId('actions-tab-recommendations'))
        .toHaveAttribute('aria-selected', 'true')
      unmount()
    }
  })

  it('lists plans on the Plans tab', () => {
    render(<Actions database="all" user={{ role: 'operator' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-plans'))
    expect(screen.getByTestId('plans-list')).toHaveTextContent('cut blocks per call')
    expect(requested).toContain('/api/v1/plans')
  })

  it('opens on the Plans tab from tab=plans', () => {
    window.location.hash = '#/actions?tab=plans'
    render(<Actions database="all" user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('actions-tab-plans')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByTestId('plans-list')).toBeInTheDocument()
  })
})
