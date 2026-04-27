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
  })
})
