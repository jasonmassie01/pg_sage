import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    data: {
      cases: [{
        case_id: 'case-1',
        title: 'Stats are stale',
        severity: 'warning',
        state: 'open',
        impact_score: 60,
        urgency_score: 75,
        why_now: 'table changed since last analyze',
        action_candidates: [{
          action_type: 'analyze_table',
          risk_tier: 'safe',
          guardrails: ['dedicated connection', 'statement_timeout'],
          policy_decision: {
            decision: 'execute',
            risk_tier: 'safe',
            requires_approval: false,
            requires_maintenance_window: false,
          },
        }],
        actions: [{
          id: 'queue:7',
          type: 'analyze_table',
          risk_tier: 'safe',
          status: 'pending',
          lifecycle_state: 'blocked',
          blocked_reason: 'action is in cooldown',
          verification_status: 'not_started',
          attempt_count: 2,
          expires_at: '2026-04-28T12:00:00Z',
          cooldown_until: '2026-04-27T12:30:00Z',
        }],
      }],
      total: 1,
    },
    loading: false,
    error: null,
    refetch: vi.fn(),
  }),
}))

describe('CasesPage', () => {
  it('shows case next step and why now', () => {
    render(<CasesPage database="all" />)

    expect(screen.getByText('Stats are stale')).toBeInTheDocument()
    expect(screen.getByText('table changed since last analyze'))
      .toBeInTheDocument()
    expect(screen.getAllByText('analyze_table').length).toBeGreaterThan(0)
    expect(screen.getByText(/execute/)).toBeInTheDocument()
    expect(screen.getByText('dedicated connection')).toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Lifecycle: blocked',
    )).toBeInTheDocument()
    expect(screen.getByText('action is in cooldown')).toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Attempts: 2',
    )).toBeInTheDocument()
  })
})
