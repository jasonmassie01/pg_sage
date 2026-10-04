import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ApprovalCard } from './ApprovalCard'

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }),
}))

// Owner decision (2026-10-04): an item Ask Sage queued shows how it was
// proposed and by whom; pg_sage's own items show nothing extra.

function card(overrides = {}) {
  return {
    queue_id: 7, database: 'orders', title: 'Analyze public.t', status: 'pending',
    action_type: 'analyze_table', targets: ['public.t'], evidence: [],
    sql: 'ANALYZE public.t', rollback: { class: 'no_rollback_needed', sql: '' },
    risk: { tier: 'safe', blast_radius: '1 table' }, why_approval: [],
    proposed_at: '2026-10-04T10:00:00Z', expires_at: '2099-01-01T00:00:00Z',
    card_hash: 'h', ...overrides,
  }
}

describe('ApprovalCard origin', () => {
  it('shows an Ask Sage origin with the asking user', () => {
    render(<ApprovalCard card={card({ origin: { via: 'ask_sage', by: 'user:42',
      label: 'Ask Sage' } })} onDecided={() => {}} />)
    expect(screen.getByTestId('approval-origin').textContent)
      .toBe('Proposed via Ask Sage by user:42')
  })

  it('shows nothing for pg_sage\'s own items', () => {
    render(<ApprovalCard card={card()} onDecided={() => {}} />)
    expect(screen.queryByTestId('approval-origin')).toBeNull()
  })

  it('renders an origin as text, never markup', () => {
    render(<ApprovalCard card={card({ origin: { via: 'ask_sage',
      by: '<img src=x onerror=alert(1)>', label: 'Ask Sage' } })} onDecided={() => {}} />)
    expect(document.querySelector('img')).toBeNull()
    expect(screen.getByTestId('approval-origin').textContent).toContain('<img')
  })
})
