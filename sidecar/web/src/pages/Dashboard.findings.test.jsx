import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Dashboard } from './Dashboard'

// G9-B08: a findings error must not read as "no recommendations", and
// "Newest recommendations" must request recency order.

vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/FleetHealthChart', () => ({
  FleetHealthChart: () => <div />,
}))
vi.mock('../components/ProviderReadinessMatrix', () => ({
  ProviderReadinessMatrix: () => <div />,
}))
vi.mock('../components/TokenBudgetBanner', () => ({
  TokenBudgetBanner: () => <div />,
}))
const urls = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    if (url.startsWith('/api/v1/findings')) {
      return { data: null, loading: false, error: '500 Internal Server Error',
        refetch: vi.fn() }
    }
    return {
      data: {
        summary: { total_databases: 1, healthy: 1, degraded: 0,
          total_critical: 0 },
        databases: [{ name: 'prod', status: 'healthy', findings_open: 0 }],
      },
      loading: false, error: null, refetch: vi.fn(),
    }
  },
}))

describe('Dashboard recent recommendations', () => {
  it('shows the findings error instead of an empty state', () => {
    render(<Dashboard database="all" onSelectDB={vi.fn()} />)
    fireEvent.click(screen.getByTestId('overview-tab-recent-recos'))
    expect(screen.getByText(/500 Internal Server Error/)).toBeInTheDocument()
    expect(screen.queryByText(/No recent recommendations/)).toBeNull()
  })

  it('requests findings ordered by recency', () => {
    render(<Dashboard database="prod db" onSelectDB={vi.fn()} />)
    const findingsURL = urls.filter(u => u.startsWith('/api/v1/findings'))
      .at(-1)
    expect(findingsURL).toContain('sort=last_seen')
    expect(findingsURL).toContain('order=desc')
    expect(findingsURL).toContain('database=prod%20db')
  })
})
