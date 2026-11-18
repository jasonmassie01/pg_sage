import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// api.overrides answers a path containing its key before the fixed
// responses below (ux-05a landing-tab tests); empty for the older tests.
const api = vi.hoisted(() => ({ overrides: {} }))

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
    const override = path && Object.keys(api.overrides).find(key => path.includes(key))
    if (override) return api.overrides[override]
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
              verification_sql: ['SELECT indisvalid FROM pg_index'],
              pr_title: 'Review pg_sage action: create_index_concurrently',
              pr_body: 'Generated from pg_sage finding 99.',
              risk_labels: ['moderate', 'create_index_concurrently'],
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
  it('describes the case to action lifecycle', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)

    expect(screen.getByTestId('actions-page-description'))
      .toHaveTextContent('Review proposed database changes')
    expect(screen.getByTestId('actions-page-description'))
      .toHaveTextContent('Cases explain the evidence')
    expect(screen.getByTestId('actions-page-description'))
      .toHaveTextContent('Approval, execution, and a verified improvement are separate steps')
  })

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
    expect(screen.getByText('Verification SQL')).toBeInTheDocument()
    expect(screen.getAllByText((_, element) => (
      element.textContent?.includes('SELECT indisvalid FROM pg_index')
    )).length).toBeGreaterThan(0)
    expect(screen.getByText('PR / CI output')).toBeInTheDocument()
    expect(screen.getByText('Review pg_sage action: create_index_concurrently'))
      .toBeInTheDocument()
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

  it('shows expired queued actions in History', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))

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

// ux-05a win 1: Actions opens on Needs your review when approvals wait and
// the role can review them; otherwise on Proposed changes. A link's tab
// wins, and once open the tab never jumps when the count changes.
const pendingRow = id => ({ id, action_type: 'analyze_table', action_risk: 'safe',
  status: 'pending', policy_decision: 'queue_for_approval', eligible: true,
  proposed_sql: `ANALYZE public.t${id}`, proposed_at: '2026-11-12T08:00:00Z' })
const pendingOf = n => ({ data: { pending: Array.from({ length: n }, (_, i) => pendingRow(i + 1)),
  total: n }, loading: false, error: null, refetch: vi.fn() })
const selected = key => screen.getByTestId(`actions-tab-${key}`).getAttribute('aria-selected')

describe('Actions landing tab', () => {
  afterEach(() => {
    api.overrides = {}
    window.location.hash = ''
  })

  it('opens on Needs your review when three approvals wait', () => {
    api.overrides = { '/api/v1/actions/pending': pendingOf(3) }
    render(<Actions database="all" user={{ role: 'operator' }} />)
    expect(selected('pending')).toBe('true')
    expect(selected('recommendations')).toBe('false')
    expect(screen.getAllByTestId('approve-button')).toHaveLength(3)
  })

  it('opens on Proposed changes when nothing waits', () => {
    api.overrides = { '/api/v1/actions/pending': pendingOf(0) }
    render(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('recommendations')).toBe('true')
    expect(selected('pending')).toBe('false')
    expect(screen.queryByTestId('actions-landing-error')).toBeNull()
  })

  it('never opens a viewer on Needs your review', () => {
    api.overrides = { '/api/v1/actions/pending': pendingOf(3) }
    render(<Actions database="all" user={{ role: 'viewer' }} />)
    expect(selected('recommendations')).toBe('true')
    expect(screen.queryByTestId('actions-tab-pending')).toBeNull()
  })

  it('opens on Proposed changes and shows the count error', () => {
    api.overrides = { '/api/v1/actions/pending': { data: null, loading: false,
      error: 'HTTP 500: queue read failed', refetch: vi.fn() } }
    render(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('recommendations')).toBe('true')
    const alert = screen.getByTestId('actions-landing-error')
    expect(alert).toHaveTextContent('HTTP 500: queue read failed')
    expect(alert).toHaveTextContent('Approvals waiting for you could not be counted')
    expect(screen.getByTestId('actions-tab-pending')).toHaveTextContent('(?)')
  })

  it('names a database whose queue could not be read', () => {
    api.overrides = { '/api/v1/actions/pending': { data: { pending: [], total: 0,
      errors: [{ database: 'staging', error: 'query failed' }] }, loading: false,
    error: null, refetch: vi.fn() } }
    render(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('recommendations')).toBe('true')
    expect(screen.getByTestId('actions-landing-error')).toHaveTextContent('staging (query failed)')
  })

  it('keeps Needs your review when the count drops from 1 to 0', () => {
    api.overrides = { '/api/v1/actions/pending': pendingOf(1) }
    const { rerender } = render(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('pending')).toBe('true')
    api.overrides = { '/api/v1/actions/pending': pendingOf(0) }
    rerender(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('pending')).toBe('true')
    expect(screen.queryAllByTestId('approve-button')).toHaveLength(0)
  })

  it('moves to Needs your review once a loading count arrives', () => {
    api.overrides = { '/api/v1/actions/pending': { data: null, loading: true, error: null,
      refetch: vi.fn() } }
    const { rerender } = render(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('recommendations')).toBe('true')
    api.overrides = { '/api/v1/actions/pending': pendingOf(2) }
    rerender(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('pending')).toBe('true')
    api.overrides = { '/api/v1/actions/pending': pendingOf(0) }
    rerender(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('pending')).toBe('true')
  })

  it('keeps a tab chosen while the count was loading', () => {
    api.overrides = { '/api/v1/actions/pending': { data: null, loading: true, error: null,
      refetch: vi.fn() } }
    const { rerender } = render(<Actions database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    api.overrides = { '/api/v1/actions/pending': pendingOf(2) }
    rerender(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('executed')).toBe('true')
  })

  it('lets a link tab win over waiting approvals', () => {
    window.location.hash = '#/actions?tab=executed'
    api.overrides = { '/api/v1/actions/pending': pendingOf(3) }
    render(<Actions database="all" user={{ role: 'admin' }} />)
    expect(selected('executed')).toBe('true')
  })
})
