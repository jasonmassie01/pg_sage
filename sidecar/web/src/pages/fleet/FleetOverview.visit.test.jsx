import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FleetOverview } from './FleetOverview'

// dashboard-12: the "new since visit" badge read a timestamp only the
// removed Findings page wrote, so it is gone. The FleetOverview neither reads
// nor listens for that key, whatever storage holds or throws.
// No concurrent access tests: one render, single-threaded.

vi.mock('../../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../../components/FleetHealthChart', () => ({
  FleetHealthChart: () => <div data-testid="fleet-health-chart" />,
}))
vi.mock('../../components/TokenBudgetBanner', () => ({
  TokenBudgetBanner: () => null,
}))
vi.mock('../../hooks/useAPI', () => ({
  useAPI: () => ({
    data: { findings: [{
      title: 'Add index', severity: 'critical', database_name: 'prod',
      first_seen: '2026-11-08T12:00:00Z',
    }] },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

const fleet = {
  data: {
    summary: { total_databases: 1, healthy: 1, degraded: 0, total_critical: 1 },
    databases: [{ name: 'prod', status: 'healthy', findings_open: 1 }],
  },
  loading: false, error: null, refetch: vi.fn(),
}

function renderDashboard() {
  return render(<FleetOverview database="all" onSelectDB={vi.fn()} fleet={fleet} />)
}

const storageDescriptor = Object.getOwnPropertyDescriptor(window, 'localStorage')

afterEach(() => {
  if (storageDescriptor) Object.defineProperty(window, 'localStorage', storageDescriptor)
  else delete window.localStorage
  vi.restoreAllMocks()
  localStorage.clear()
})

describe('FleetOverview without the visit badge', () => {
  it('renders no new-since-visit text when localStorage throws', () => {
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      get() { throw new DOMException('blocked', 'SecurityError') },
    })
    renderDashboard()
    expect(screen.getByText('Critical Issues')).toBeInTheDocument()
    expect(screen.queryByTestId('new-since-visit-badge')).toBeNull()
    expect(screen.queryByText(/new$/)).toBeNull()
  })

  it('ignores an old visit timestamp left in storage', () => {
    localStorage.setItem('pg_sage_' + 'findings_visit',
      String(Date.parse('2026-11-01T00:00:00Z')))
    renderDashboard()
    expect(screen.getByText('Critical Issues')).toBeInTheDocument()
    expect(screen.queryByTestId('new-since-visit-badge')).toBeNull()
    expect(screen.queryByText(/\+\d+ new/)).toBeNull()
  })

  it('adds no storage listener', () => {
    const add = vi.spyOn(window, 'addEventListener')
    renderDashboard()
    expect(add.mock.calls.map(([type]) => type)).not.toContain('storage')
  })
})
