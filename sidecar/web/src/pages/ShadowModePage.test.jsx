import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ShadowModePage } from './ShadowModePage'

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    data: {
      total_cases: 14,
      would_auto_resolve: 12,
      requires_approval: 2,
      blocked: 1,
      estimated_toil_minutes: 360,
      blocked_reasons: ['requires approval'],
      proof: [
        {
          case_id: 'case-1',
          title: 'Stats are stale',
          action_type: 'analyze_table',
          policy_decision: 'execute',
          status: 'success',
          verification_status: 'verified',
          estimated_toil_minutes: 15,
        },
        {
          case_id: 'case-2',
          title: 'Needs DDL',
          action_type: 'create_index_concurrently',
          policy_decision: 'queue_for_approval',
          estimated_toil_minutes: 30,
          blocked_reason: 'requires approval',
        },
      ],
    },
    loading: false,
    error: null,
  }),
}))

describe('ShadowModePage', () => {
  it('shows avoided toil and auto-safe preview', () => {
    render(<ShadowModePage database="all" />)

    expect(screen.getByText('14')).toBeInTheDocument()
    expect(screen.getByText('12')).toBeInTheDocument()
    expect(screen.getByText('360 min')).toBeInTheDocument()
    expect(screen.getByText('Stats are stale')).toBeInTheDocument()
    expect(screen.getByText('analyze_table')).toBeInTheDocument()
    expect(screen.getByText('Status: success')).toBeInTheDocument()
    expect(screen.getByText('Verification: verified')).toBeInTheDocument()
    expect(screen.getAllByText(/requires approval/).length)
      .toBeGreaterThan(0)
  })
})
