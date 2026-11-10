import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Dashboard } from './Dashboard'

vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/FleetHealthChart', () => ({
  FleetHealthChart: ({ database }) => <div data-testid="fleet-health-chart">{database}</div>,
}))
vi.mock('../components/ProviderReadinessMatrix', () => ({ ProviderReadinessMatrix: () => <div /> }))
vi.mock('../components/TokenBudgetBanner', () => ({ TokenBudgetBanner: () => <div /> }))
vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: null, loading: false, error: null, refetch: vi.fn() }),
}))

const fleet = {
  data: { summary: { total_databases: 1, healthy: 1, degraded: 0, total_critical: 0 },
    databases: [{ name: 'prod', status: 'healthy', findings_open: 0 }] },
  loading: false, error: null, refetch: vi.fn(),
}

// dashboard-09: recharts lives in a lazily loaded chunk, so the chart slot
// first shows a skeleton and then the chart with the selected database.
describe('Dashboard fleet health chart', () => {
  it('shows a placeholder, then the lazily loaded chart for the database', async () => {
    render(<Dashboard database="prod" onSelectDB={vi.fn()} fleet={fleet} />)
    expect(screen.getByTestId('fleet-health-chart-loading')).toBeInTheDocument()
    expect(screen.queryByTestId('fleet-health-chart')).not.toBeInTheDocument()
    expect(await screen.findByTestId('fleet-health-chart')).toHaveTextContent('prod')
    expect(screen.queryByTestId('fleet-health-chart-loading')).not.toBeInTheDocument()
  })
})
