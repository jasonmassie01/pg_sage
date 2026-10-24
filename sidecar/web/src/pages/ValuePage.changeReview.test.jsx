import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ValuePage } from './ValuePage'

// The Value page names the Change Review credit (by_feature
// "change_review", final minutes / 60) "Change Review".

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    data: {
      dba_hours_saved: { all_time: 0.25, this_month: 0.25, this_week: 0.25 },
      by_feature: { change_review: 0.25 },
      by_database: [{ name: 'orders', hours: 0.25 }],
      incidents_avoided: { count: 0, credited_hours: 0, detail: [] },
      potential_hours_pending: 0.5,
      trend_daily: [{ day: '2026-10-23', hours: 0.25 }],
    },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

describe('ValuePage Change Review credit', () => {
  it('labels by_feature change_review as Change Review', () => {
    render(<ValuePage database="all" />)
    const features = screen.getByRole('table', { name: /hours saved by feature/i })
    const row = within(features).getByRole('rowheader', { name: 'Change Review' })
    expect(row.closest('tr')).toHaveTextContent('0.3 h')
    expect(within(features).queryByText('change_review')).toBeNull()
    expect(within(features).queryByText('Change review')).toBeNull()
  })
})
