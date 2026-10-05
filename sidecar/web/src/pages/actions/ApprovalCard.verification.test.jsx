import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ApprovalCard } from './ApprovalCard'

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }),
}))

// One change per object: a card whose object has a verification in flight
// says so, and that approving it overrides that verification.

function card(overrides = {}) {
  return {
    queue_id: 61, database: 'lifeos', title: 'Index on public.memories',
    action_type: 'create_index_concurrently', status: 'pending',
    targets: ['public.memories'], evidence: [], rationale: null, predicted_effect: {},
    sql: 'CREATE INDEX CONCURRENTLY idx_memories_status_type_quality_current ON ' +
      'public.memories (status, fact_type, quality_score)',
    rollback: { class: 'reversible', sql: 'DROP INDEX CONCURRENTLY idx' },
    risk: { tier: 'moderate', blast_radius: '1 table', lock: 'none', guardrails: [],
      post_checks: [] },
    why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
    proposed_at: '2026-10-04T18:55:00Z',
    expires_at: new Date(Date.now() + 3600 * 1000).toISOString(),
    card_hash: 'hash-61',
    ...overrides,
  }
}

describe('ApprovalCard verification wait', () => {
  it('shows the pending verification and the override', () => {
    const line = 'Awaiting verification of action 6410 (until 2026-10-04 19:45 UTC): ' +
      'approving overrides pending verification of action 6410'
    render(<ApprovalCard card={card({ verification_wait: { action_ids: [6410],
      objects: ['table:public.memories'], until: '2026-10-04T19:45:00Z', line } })} />)
    const wait = screen.getByTestId('approval-verification-wait')
    expect(wait).toHaveTextContent('Awaiting verification of action 6410')
    expect(wait).toHaveTextContent('approving overrides pending verification of action 6410')
  })

  it('shows an unreadable wait as unavailable', () => {
    render(<ApprovalCard card={card({ verification_wait: { action_ids: [],
      objects: [], unavailable: 'boom',
      line: 'Verification wait: unavailable (boom)' } })} />)
    expect(screen.getByTestId('approval-verification-wait'))
      .toHaveTextContent('unavailable (boom)')
  })

  it('shows nothing without a wait', () => {
    render(<ApprovalCard card={card()} />)
    expect(screen.queryByTestId('approval-verification-wait')).toBeNull()
  })
})
