import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ApprovalCard } from './ApprovalCard'

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }),
}))

// policy-04: a card the maintenance window alone withholds can still be
// approved; the approval is scheduled for the next opening. Any other
// refusal keeps Approve disabled.

const card = {
  queue_id: 62, database: 'orders', title: 'Index on public.t',
  action_type: 'create_index_concurrently', status: 'pending', targets: ['public.t'],
  evidence: [], rationale: null, predicted_effect: {},
  sql: 'CREATE INDEX CONCURRENTLY i ON public.t (c)',
  rollback: { class: 'reversible', sql: 'DROP INDEX CONCURRENTLY i' },
  risk: { tier: 'moderate', blast_radius: '1 table', lock: 'none', guardrails: [],
    post_checks: [] },
  why_approval: [], proposed_at: '2026-11-11T14:00:00Z',
  expires_at: new Date(Date.now() + 3600 * 1000).toISOString(), card_hash: 'hash-62',
}

describe('ApprovalCard outside the maintenance window', () => {
  it('offers approval for the next window when only the window refuses', () => {
    render(<ApprovalCard card={card} eligible={false} windowOnly
      blockedReason="outside maintenance window" />)
    const approve = screen.getByTestId('approval-approve')
    expect(approve).toBeEnabled()
    expect(approve).toHaveTextContent('Approve for the next window')
    expect(screen.getByTestId('approval-blocker')).toHaveTextContent(/schedules it for the next opening/)
  })

  it('keeps Approve disabled for any other refusal', () => {
    render(<ApprovalCard card={card} eligible={false} windowOnly={false}
      blockedReason="change class not allowed" />)
    const approve = screen.getByTestId('approval-approve')
    expect(approve).toBeDisabled()
    expect(approve).toHaveTextContent('Approve and run')
  })

  it('keeps Approve disabled in Observation mode even when only the window refuses', () => {
    render(<ApprovalCard card={card} eligible={false} windowOnly mode="observation" />)
    expect(screen.getByTestId('approval-approve')).toBeDisabled()
  })
})
