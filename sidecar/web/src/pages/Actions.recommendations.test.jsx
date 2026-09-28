import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// Durable recommendation state machine (MASTER-SPEC 5.1 #5): the Actions
// page shows each recommendation's state and revision, and a queued
// proposal shows the revision its approval would pin.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const recommendations = [
  {
    id: 12, state: 'verifying', revision: 3, category: 'missing_index',
    target: 'public.orders', action_type: 'create_index',
    approved_by: 'policy:decision:9', approved_revision: 3, attempt_count: 1,
    retry_budget: 2, verdict: 'regressed', reason: '',
    updated_at: '2026-09-27T10:00:00Z',
  },
  {
    id: 13, state: 'abandoned', revision: 1, category: 'vacuum',
    target: 'public.items', action_type: 'vacuum', approved_by: 'user:4',
    approved_revision: 1, attempt_count: 3, retry_budget: 2, verdict: '',
    reason: 'retry budget exhausted after 3 attempts: lock timeout',
    updated_at: '2026-09-27T09:00:00Z',
  },
]

const pendingRow = {
  id: 44, database_name: 'prod', finding_id: 5, action_risk: 'safe',
  proposed_sql: 'CREATE INDEX CONCURRENTLY i ON t (a)', status: 'pending',
  proposed_at: '2026-09-27T10:00:00Z', recommendation_id: 12,
  recommendation_revision: 3,
}

const seen = []
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => {
    seen.push(path)
    let data = { actions: [], total: 0 }
    if (path?.includes('/recommendations')) {
      data = { database: 'prod', recommendations }
    } else if (path?.includes('/pending')) {
      data = { pending: [pendingRow], total: 1 }
    }
    return { data, loading: false, error: null, refetch: vi.fn() }
  },
}))

describe('Recommendations tab', () => {
  it('lists each recommendation with its state and revision', () => {
    render(<Actions database="prod" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-recommendations'))
    expect(seen).toContain('/api/v1/recommendations?database=prod')
    const table = screen.getByTestId('recommendations-table')
    expect(table).toHaveTextContent('Verifying')
    expect(table).toHaveTextContent('r3')
    expect(table).toHaveTextContent('Abandoned')
    expect(table).toHaveTextContent('regressed')
    expect(table).toHaveTextContent(
      'retry budget exhausted after 3 attempts: lock timeout')
    expect(table).toHaveTextContent('policy:decision:9')
  })

  it('asks for one database when the fleet view is selected', () => {
    render(<Actions database="all" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-recommendations'))
    expect(screen.getByTestId('recommendations-select-database'))
      .toBeInTheDocument()
    expect(screen.queryByTestId('recommendations-table')).toBeNull()
  })

  it('shows the revision a pending approval pins', () => {
    render(<Actions database="prod" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-pending'))
    const table = screen.getByTestId('pending-actions-table')
    expect(table).toHaveTextContent('#12 r3')
  })
})
