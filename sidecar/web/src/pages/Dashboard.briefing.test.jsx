import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Dashboard } from './Dashboard'

// llm-06b: the Overview shows the latest briefing card for the selection.
// No concurrent access tests: one synchronous render with mocked queries.

vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/FleetHealthChart', () => ({ FleetHealthChart: () => <div /> }))
vi.mock('../components/ProviderReadinessMatrix', () => ({
  ProviderReadinessMatrix: () => <div />,
}))
vi.mock('../components/TokenBudgetBanner', () => ({ TokenBudgetBanner: () => <div /> }))

const urls = vi.hoisted(() => [])
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    if (url.startsWith('/api/v1/briefings/latest')) {
      return {
        data: null, loading: false, error: 'no briefing yet: none is stored',
        errorStatus: 404, refetch: vi.fn(),
      }
    }
    return { data: { findings: [] }, loading: false, error: null, refetch: vi.fn() }
  },
}))

const fleet = {
  data: {
    summary: { total_databases: 1, healthy: 1, degraded: 0, total_critical: 0 },
    databases: [{ name: 'prod', status: 'healthy', findings_open: 0 }],
  },
  loading: false, error: null, refetch: vi.fn(),
}

describe('Dashboard briefing card', () => {
  it('shows no briefing yet for the selected database', () => {
    render(<Dashboard database="prod" onSelectDB={vi.fn()} fleet={fleet} />)
    expect(screen.getByTestId('briefing-card')).toBeInTheDocument()
    expect(screen.getByTestId('briefing-empty')).toHaveTextContent('No briefing yet')
    expect(urls).toContain('/api/v1/briefings/latest?database=prod')
  })
})
