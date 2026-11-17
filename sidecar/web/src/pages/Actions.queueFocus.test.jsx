import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// ux-01b: #/actions?tab=pending&queue=<id> opens Needs your review with the
// queued proposal's card scrolled to and outlined. A hostile queue= or an
// unknown tab= changes nothing. No concurrent access tests: one render.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const pendingRow = id => ({ id, database_name: 'orders', action_type: 'analyze_table',
  action_risk: 'safe', status: 'pending', proposed_sql: 'ANALYZE public.orders',
  proposed_at: '2026-10-03T11:00:00Z' })
const card = id => ({ queue_id: id, database: 'orders', title: `Card ${id}`,
  action_type: 'analyze_table', status: 'pending', targets: [], evidence: [],
  predicted_effect: {}, sql: 'ANALYZE public.orders', rollback: { class: 'no_rollback_needed' },
  risk: { tier: 'safe', guardrails: [], post_checks: [] }, why_approval: [],
  expires_at: '2026-10-04T11:00:00Z', card_hash: `h${id}` })

vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => {
    const ok = data => ({ data, loading: false, error: null, refetch: vi.fn() })
    if (path?.startsWith('/api/v1/approvals')) {
      return ok({ cards: [card(41), card(42)], total: 2, errors: [] })
    }
    if (path?.includes('/api/v1/actions/pending')) {
      return ok({ pending: [pendingRow(41), pendingRow(42)], total: 2 })
    }
    return ok({ actions: [], total: 0 })
  },
}))

const scrolled = vi.fn()
function renderAt(hash) {
  window.location.hash = hash
  Element.prototype.scrollIntoView = scrolled
  render(<Actions database="orders" user={{ role: 'operator' }} />)
}

afterEach(() => {
  window.location.hash = ''
  scrolled.mockReset()
  delete Element.prototype.scrollIntoView
})

describe('Actions exact links', () => {
  it('opens tab= and outlines the queue= card', () => {
    renderAt('#/actions?tab=pending&queue=42&database=orders')
    expect(screen.getByTestId('actions-tab-pending')).toHaveAttribute('aria-selected', 'true')
    const focused = screen.getByTestId('approval-card-focused')
    expect(focused).toHaveTextContent('Card 42')
    expect(focused).not.toHaveTextContent('Card 41')
    expect(screen.getAllByTestId('approval-card-focused')).toHaveLength(1)
    expect(scrolled).toHaveBeenCalledTimes(1)
  })

  it('ignores a hostile queue id and an unknown tab', () => {
    renderAt('#/actions?tab=drafts&queue=42%3Cimg%3E')
    // ux-05a: an ignored tab= falls to the landing tab, which is Needs your
    // review here because two approvals wait.
    expect(screen.getByTestId('actions-tab-pending'))
      .toHaveAttribute('aria-selected', 'true')
    expect(screen.queryByTestId('approval-card-focused')).toBeNull()
  })

  it('opens the History tab from tab=executed', () => {
    renderAt('#/actions?tab=executed')
    expect(screen.getByTestId('actions-tab-executed')).toHaveAttribute('aria-selected', 'true')
  })
})
