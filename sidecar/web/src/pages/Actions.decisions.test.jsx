import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// policy-07: the Decisions tab is offered to operators and admins only;
// a viewer gets no decision data (the route refuses viewers too).

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
    const decisions = { database: 'all', partial: [], truncated: false, limit: 100,
      decisions: [{ database: 'alpha', decision_id: 3, verdict: 'parked',
        reason: 'budget_exceeded', human_reason: 'budget exceeded', repeat_count: 2,
        target_objects: [], last_seen_at: '2026-11-10T10:00:00Z' }] }
    return { data: path?.startsWith('/api/v1/decisions') ? decisions
      : { actions: [], pending: [], total: 0 }, loading: false, error: null,
    refetch: vi.fn() }
  },
}))

describe('Actions > Decisions', () => {
  it('shows an operator the withheld decisions', () => {
    render(<Actions database="all" user={{ role: 'operator' }} />)
    // ux-05b: the tab reads "Held back"; its key and test id stay.
    expect(screen.getByTestId('actions-tab-decisions')).toHaveTextContent(/^Held back/)
    expect(screen.getByRole('tab', { name: /^Held back/ })).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('actions-tab-decisions'))
    expect(screen.getByTestId('decision-row')).toHaveTextContent('budget exceeded')
    expect(requested).toContain('/api/v1/decisions')
  })

  it('offers a viewer no Decisions tab and reads no decisions', () => {
    requested.length = 0
    render(<Actions database="all" user={{ role: 'viewer' }} />)
    expect(screen.queryByTestId('actions-tab-decisions')).toBeNull()
    expect(requested.some(p => p?.startsWith('/api/v1/decisions'))).toBe(false)
  })
})
