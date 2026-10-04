import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ApprovalCard } from './ApprovalCard'

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }),
}))

// Roadmap 1.4: an approval card whose class has shadow history shows it.

function card(overrides = {}) {
  return {
    queue_id: 52, database: 'orders', title: 'Vacuum public.orders',
    action_type: 'vacuum_table', status: 'pending', targets: ['public.orders'],
    evidence: [], rationale: null, predicted_effect: {},
    sql: 'VACUUM public.orders', rollback: { class: 'no_rollback_needed', sql: '' },
    risk: { tier: 'safe', blast_radius: '1 table', lock: 'none', guardrails: [],
      post_checks: [] },
    why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
    proposed_at: '2026-10-03T11:00:00Z',
    expires_at: new Date(Date.now() + 3600 * 1000).toISOString(),
    card_hash: 'hash-52',
    ...overrides,
  }
}

describe('ApprovalCard shadow history', () => {
  it('shows the class shadow history when there is one', () => {
    render(<ApprovalCard card={card({ shadow_history: { family: 'hygiene', class: 'vacuum',
      summary: { total: 4, pending: 1, correct: 3, incorrect: 0, neutral: 0, unscored: 0,
        counted: 2 }, this_proposal: null } })} />)
    expect(screen.getByTestId('approval-shadow')).toHaveTextContent(/4 shadow decisions/)
  })

  it('shows nothing when the class has none', () => {
    render(<ApprovalCard card={card()} />)
    expect(screen.queryByTestId('approval-shadow')).toBeNull()
  })
})
