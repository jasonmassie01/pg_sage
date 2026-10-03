import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// The Pending Approval tab leads with approval cards (the why, evidence,
// SQL, rollback, one-click decisions); the compact table stays one click
// away. Without cards (older sidecar, API error) the table is shown.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const pendingRow = {
  id: 41, database_name: 'orders', action_type: 'analyze_table', action_risk: 'safe',
  finding_id: 12, status: 'pending', proposed_sql: 'ANALYZE public.orders',
  proposed_at: '2026-10-03T11:00:00Z',
}

const approvalCard = {
  queue_id: 41, database: 'orders', title: 'Stale statistics on public.orders',
  action_type: 'analyze_table', status: 'pending', targets: ['public.orders'],
  evidence: [{ kind: 'metric', label: 'n mod since analyze', value: '5000', ref: 'finding:12' }],
  rationale: null, predicted_effect: {}, sql: 'ANALYZE public.orders',
  rollback: { class: 'no_rollback_needed', sql: '', note: 'Nothing to undo' },
  risk: { tier: 'safe', blast_radius: '1 table', lock: 'SHARE UPDATE EXCLUSIVE',
    guardrails: [], post_checks: [] },
  why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
  expires_at: '2026-10-04T11:00:00Z', card_hash: 'h',
}

let cardsResponse = { cards: [approvalCard], total: 1, errors: [] }

vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => {
    if (path?.startsWith('/api/v1/approvals/41')) {
      return { data: { card: approvalCard, eligible: true }, loading: false, error: null,
        refetch: vi.fn() }
    }
    if (path?.startsWith('/api/v1/approvals')) {
      return { data: cardsResponse, loading: false, error: null, refetch: vi.fn() }
    }
    if (path?.includes('/api/v1/actions/pending')) {
      return { data: { pending: [pendingRow], total: 1 }, loading: false, error: null,
        refetch: vi.fn() }
    }
    return { data: { actions: [], total: 0 }, loading: false, error: null, refetch: vi.fn() }
  },
}))

function openPending() {
  render(<Actions database="all" user={{ role: 'operator' }} />)
  fireEvent.click(screen.getByText('Pending Approval'))
}

describe('Actions pending approval cards', () => {
  it('leads with the approval cards and keeps the table one click away', () => {
    cardsResponse = { cards: [approvalCard], total: 1, errors: [] }
    openPending()
    expect(screen.getByTestId('approval-card')).toBeInTheDocument()
    expect(screen.getByTestId('approval-why')).toHaveTextContent('Trust level is advisory')
    expect(screen.queryByTestId('pending-actions-table')).toBeNull()
    fireEvent.click(screen.getByTestId('pending-view-table'))
    expect(screen.getByTestId('pending-actions-table')).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('pending-view-cards'))
    expect(screen.getByTestId('approval-card')).toBeInTheDocument()
  })

  it('falls back to the table when the cards API returns nothing usable', () => {
    cardsResponse = { actions: [] }
    openPending()
    expect(screen.queryByTestId('approval-card')).toBeNull()
    expect(screen.getByTestId('pending-actions-table')).toBeInTheDocument()
  })

  it('shows the card in the action detail of the table view', () => {
    cardsResponse = { actions: [] }
    openPending()
    fireEvent.click(screen.getByTestId('row-expander'))
    const detail = screen.getByTestId('approval-card-detail')
    expect(detail).toHaveTextContent('Why it needs you')
    expect(detail).toHaveTextContent('Trust level is advisory')
  })

  it('shows an empty state when there are no cards', () => {
    cardsResponse = { cards: [], total: 0, errors: [] }
    openPending()
    expect(screen.queryByTestId('approval-card')).toBeNull()
    expect(screen.getByTestId('approval-cards-empty')).toBeInTheDocument()
  })
})
