import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { DatabasePage } from './DatabasePage'

// ux-11: the health chart moved from Snapshot & metrics to the Database
// page. dashboard-09: recharts lives in a lazily loaded chunk, so the chart
// slot first shows a skeleton and then the chart for the database.
// No concurrent access tests: one render tree.

vi.mock('../components/FleetHealthChart', () => ({
  FleetHealthChart: ({ database }) => <div data-testid="fleet-health-chart">{database}</div>,
}))
vi.mock('./databases/DatabaseSummary', () => ({ DatabaseSummary: () => <div /> }))
vi.mock('../components/RecoverabilityPanel', () => ({ RecoverabilityPanel: () => <div /> }))
vi.mock('./database/EvidencePacks', () => ({ EvidencePacks: () => <div /> }))
vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: { snapshot: { tables: 1 } }, loading: false, error: null,
    refetch: vi.fn() }),
}))

describe('DatabasePage health chart', () => {
  it('shows a placeholder, then the lazily loaded chart for the database', async () => {
    render(<DatabasePage database="prod" />)
    expect(screen.getByTestId('fleet-health-chart-loading')).toBeInTheDocument()
    expect(screen.queryByTestId('fleet-health-chart')).not.toBeInTheDocument()
    expect(await screen.findByTestId('fleet-health-chart')).toHaveTextContent('prod')
    expect(screen.queryByTestId('fleet-health-chart-loading')).not.toBeInTheDocument()
  })

  it('draws no chart without one selected database', () => {
    render(<DatabasePage database="all" />)
    expect(screen.queryByTestId('fleet-health-chart-loading')).toBeNull()
    expect(screen.queryByTestId('fleet-health-chart')).toBeNull()
  })
})
