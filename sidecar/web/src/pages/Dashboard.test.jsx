import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Dashboard } from './Dashboard'
import { useAPI } from '../hooks/useAPI'

vi.mock('../hooks/useLiveEvents', () => ({
  useLiveRefetch: vi.fn(),
}))

vi.mock('../components/FleetHealthChart', () => ({
  FleetHealthChart: () => <div data-testid="fleet-health-chart" />,
}))

vi.mock('../components/ProviderReadinessMatrix', () => ({
  ProviderReadinessMatrix: () => <div data-testid="provider-readiness-matrix" />,
}))

vi.mock('../components/TokenBudgetBanner', () => ({
  TokenBudgetBanner: () => <div data-testid="token-budget-banner" />,
}))

vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    if (url && url.startsWith('/api/v1/cases')) {
      return {
        data: {
          cases: [{
            id: 'finding:prod:gin',
            title: 'Add GIN index for JSON containment query',
            severity: 'warning',
            database_name: 'prod',
          }],
          total: 1,
          unavailable: [],
        },
        loading: false,
        error: null,
        refetch: vi.fn(),
      }
    }

    return {
      data: {
        summary: {
          total_databases: 2,
          healthy: 1,
          degraded: 1,
          total_critical: 0,
        },
        databases: [{
          name: 'prod',
          status: 'healthy',
          findings_open: 0,
        }, {
          name: 'analytics',
          status: 'degraded',
          findings_open: 3,
        }],
      },
      loading: false,
      error: null,
      refetch: vi.fn(),
    }
  },
}))

describe('Dashboard', () => {
  it('defaults overview tabs to databases and describes each overview tab', () => {
    render(<Dashboard database="all" onSelectDB={vi.fn()}
      fleet={useAPI('/api/v1/databases')} />)

    expect(screen.getByTestId('overview-tab-databases'))
      .toHaveAttribute('aria-selected', 'true')
    expect(screen.getByTestId('overview-tab-description'))
      .toHaveTextContent(/registered Postgres databases/i)
    expect(screen.getByTestId('db-list')).toBeInTheDocument()
    expect(screen.getByTestId('db-list')).toHaveTextContent('analytics')
    expect(screen.queryByTestId('provider-readiness-matrix'))
      .not.toBeInTheDocument()

    fireEvent.click(screen.getByTestId('overview-tab-provider-readiness'))
    expect(screen.getByTestId('overview-tab-provider-readiness'))
      .toHaveAttribute('aria-selected', 'true')
    expect(screen.getByTestId('overview-tab-description'))
      .toHaveTextContent(/cloud provider credentials/i)
    expect(screen.getByTestId('provider-readiness-matrix'))
      .toBeInTheDocument()
    expect(screen.queryByTestId('db-list')).not.toBeInTheDocument()

    fireEvent.click(screen.getByTestId('overview-tab-top-cases'))
    expect(screen.getByTestId('overview-tab-description'))
      .toHaveTextContent(/highest-ranked open issues/i)
    expect(screen.getByTestId('top-cases')).toBeInTheDocument()
  })
})
