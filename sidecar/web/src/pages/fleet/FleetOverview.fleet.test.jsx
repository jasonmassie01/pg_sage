import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FleetOverview } from './FleetOverview'
import { LiveEventsProvider } from '../../hooks/useLiveEvents'
import { FakeEventSource } from '../../test/fakeEventSource'
import { stubFetch } from '../../test/fetchStub'

// dashboard-08: FleetOverview renders the fleet App already polls (the fleet
// query is a prop) and fetches no /api/v1/databases of its own. The real
// useAPI runs against stubFetch; only the self-fetching panels are stubbed.
// No concurrent access tests: one render, single-threaded timers.

vi.mock('../../components/FleetHealthChart', () => ({ FleetHealthChart: () => <div /> }))
vi.mock('../../components/ProviderReadinessMatrix', () => ({
  ProviderReadinessMatrix: () => <div />,
}))
vi.mock('../../components/TokenBudgetBanner', () => ({ TokenBudgetBanner: () => <div /> }))

const fleetData = {
  summary: { total_databases: 2, healthy: 1, degraded: 1, total_critical: 0 },
  databases: [
    { name: 'prod', status: 'healthy', findings_open: 0 },
    { name: 'analytics', status: 'degraded', findings_open: 3 },
  ],
}
const fleetQuery = (extra = {}) => ({
  data: fleetData, loading: false, error: null, errorStatus: null, stale: false,
  updatedAt: Date.now(), failedAt: null, refetch: vi.fn(), ...extra,
})

let fetch
beforeEach(() => {
  vi.useFakeTimers()
  fetch = stubFetch({ '/api/v1/cases': { json: { cases: [], total: 0, unavailable: [] } } })
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const advance = ms => act(async () => { await vi.advanceTimersByTimeAsync(ms) })
const fetchedPaths = () => fetch.mock.calls.map(c => String(c[0]).split('?')[0])

describe('FleetOverview fleet', () => {
  it('renders from the fleet prop and fetches no databases list', async () => {
    render(<FleetOverview database="all" onSelectDB={vi.fn()} fleet={fleetQuery()} />)
    await advance(60000)
    expect(screen.getByTestId('stat-databases')).toHaveTextContent('2')
    expect(screen.getByTestId('stat-healthy')).toHaveTextContent('1')
    expect(fetchedPaths()).not.toContain('/api/v1/databases')
    // cases-11b: the ranked cases load with their tab, not with the page.
    expect(fetchedPaths()).not.toContain('/api/v1/cases')
  })

  it('shows the fleet error when App has no fleet data', async () => {
    const fleet = fleetQuery({ data: null, error: 'proxy down (502)', errorStatus: 502 })
    render(<FleetOverview database="all" onSelectDB={vi.fn()} fleet={fleet} />)
    await advance(0)
    expect(screen.getAllByText(/proxy down \(502\)/).length).toBeGreaterThan(0)
    expect(screen.queryByTestId('stat-databases')).toBeNull()
  })

  it('keeps the page under a failed fleet refresh and shows the strip', async () => {
    const fleet = fleetQuery({ error: 'proxy down (502)', stale: true,
      failedAt: Date.now() })
    render(<FleetOverview database="all" onSelectDB={vi.fn()} fleet={fleet} />)
    await advance(0)
    expect(screen.getByTestId('query-stale')).toBeInTheDocument()
    expect(screen.getByTestId('stat-databases')).toHaveTextContent('2')
  })

  it('treats a missing fleet prop as loading, not as an empty fleet', async () => {
    render(<FleetOverview database="all" onSelectDB={vi.fn()} />)
    await advance(0)
    expect(screen.queryByTestId('onboarding-welcome')).toBeNull()
    expect(fetchedPaths()).not.toContain('/api/v1/databases')
  })

  it('refetches App\'s fleet on a health event for any database', async () => {
    const fleet = fleetQuery()
    render(
      <LiveEventsProvider>
        <FleetOverview database="prod" onSelectDB={vi.fn()} fleet={fleet} />
      </LiveEventsProvider>,
    )
    await advance(0)
    const source = FakeEventSource.instances.at(-1)
    act(() => source.open())
    const before = fetch.mock.calls.length
    act(() => source.emit('health', { type: 'health', database: 'analytics' }))
    await advance(2000)
    expect(fleet.refetch).toHaveBeenCalledTimes(1)
    // A health event refetches the fleet only, not the cases list.
    expect(fetch.mock.calls.length).toBe(before)
  })

  it('refetches the scoped top cases only for the selected database', async () => {
    render(
      <LiveEventsProvider>
        <FleetOverview database="prod" onSelectDB={vi.fn()} fleet={fleetQuery()} />
      </LiveEventsProvider>,
    )
    await advance(0)
    act(() => screen.getByTestId('overview-tab-top-cases').click())
    await advance(0)
    const source = FakeEventSource.instances.at(-1)
    act(() => source.open())
    const before = fetch.mock.calls.length
    act(() => source.emit('findings', { type: 'findings', database: 'analytics' }))
    await advance(2000)
    expect(fetch.mock.calls.length).toBe(before)
    act(() => source.emit('findings', { type: 'findings', database: 'prod' }))
    await advance(2000)
    expect(fetch.mock.calls.length).toBe(before + 1)
    expect(fetch.mock.calls.at(-1)[0]).toContain('database=prod')
    expect(String(fetch.mock.calls.at(-1)[0])).toMatch(/^\/api\/v1\/cases\?/)
  })
})
