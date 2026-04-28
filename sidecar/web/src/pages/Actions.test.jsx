import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))

vi.mock('../hooks/useLiveEvents', () => ({
  useLiveRefetch: vi.fn(),
}))

vi.mock('../components/Layout', () => ({
  usePendingActionsRefetch: () => vi.fn(),
}))

vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => {
    if (path?.includes('/api/v1/actions/pending')) {
      return {
        data: {
          pending: [{
            id: 12,
            action_type: 'create_index_concurrently',
            action_risk: 'moderate',
            finding_id: 99,
            status: 'pending',
            policy_decision: 'queue_for_approval',
            eligible: false,
            defer_reason: 'outside maintenance window',
            rollback_class: 'forward_fix_only',
            proposed_sql: 'CREATE INDEX CONCURRENTLY idx_orders_customer ON orders(customer_id)',
            rollback_sql: 'DROP INDEX CONCURRENTLY idx_orders_customer',
            proposed_at: '2026-04-28T12:00:00Z',
            script_output: {
              filename: '99_create_index_concurrently.sql',
              migration_sql: 'CREATE INDEX CONCURRENTLY idx_orders_customer ON orders(customer_id)',
              rollback_sql: 'DROP INDEX CONCURRENTLY idx_orders_customer',
              format: 'sql',
            },
          }],
          total: 1,
        },
        loading: false,
        error: null,
        refetch: vi.fn(),
      }
    }
    return {
      data: {
        actions: [{
          id: '22',
          action_type: 'analyze_table',
          sql_executed: 'ANALYZE public.orders',
          outcome: 'expired',
          rollback_reason: 'action proposal expired',
          executed_at: '2026-04-28T12:00:00Z',
        }],
        total: 1,
      },
      loading: false,
      error: null,
      refetch: vi.fn(),
    }
  },
}))

describe('Actions', () => {
  it('shows generated migration script output for pending DDL', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)

    fireEvent.click(screen.getByTestId('actions-tab-pending'))
    fireEvent.click(screen.getByLabelText('Expand row'))

    expect(screen.getByText('Migration script')).toBeInTheDocument()
    expect(screen.getByText('99_create_index_concurrently.sql'))
      .toBeInTheDocument()
    expect(screen.getAllByText(/CREATE INDEX CONCURRENTLY/).length)
      .toBeGreaterThan(0)
    expect(screen.getByText('Rollback script')).toBeInTheDocument()
    expect(screen.getAllByText((_, element) => (
      element.textContent?.includes('DROP INDEX CONCURRENTLY idx_orders_customer')
    )).length)
      .toBeGreaterThan(0)
  })

  it('disables approval for deferred pending actions', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)

    fireEvent.click(screen.getByTestId('actions-tab-pending'))

    const approve = screen.getByTestId('approve-button')
    expect(approve).toBeDisabled()
    expect(approve).toHaveAttribute('title', 'outside maintenance window')
  })

  it('shows rollback class before approval', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)

    fireEvent.click(screen.getByTestId('actions-tab-pending'))
    fireEvent.click(screen.getByLabelText('Expand row'))

    expect(screen.getByText('Rollback: forward fix only'))
      .toBeInTheDocument()
  })

  it('shows expired queued actions in the action ledger', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)

    expect(screen.getByText('Expired')).toBeInTheDocument()
    expect(screen.getByText('action proposal expired')).toBeInTheDocument()
    fireEvent.click(screen.getByLabelText('Expand row'))
    expect(screen.getByText('Proposed SQL')).toBeInTheDocument()
    expect(screen.getAllByText((_, element) => (
      element.textContent?.includes('ANALYZE public.orders')
    )).length)
      .toBeGreaterThan(0)
  })
})
