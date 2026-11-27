import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { FleetOverview } from './FleetOverview'

// cases-11: the Overview hero claims health only when no critical or
// warning work is open and collection evidence is fresh. Every count it
// cannot read blocks the claim.
// The "+N new" badge this item once covered was removed by dashboard-12
// (FleetOverview.visit.test.jsx pins its absence), so no badge test here.
// No concurrent access tests: one render at a time, single-threaded.

vi.mock('../../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../../components/FleetHealthChart', () => ({
  FleetHealthChart: () => <div data-testid="fleet-health-chart" />,
}))
vi.mock('../../components/TokenBudgetBanner', () => ({
  TokenBudgetBanner: () => null,
}))
vi.mock('../../hooks/useAPI', () => ({
  useAPI: () => ({
    data: { findings: [] }, loading: false, error: null, refetch: vi.fn(),
  }),
}))

const CLEAR = {
  total_databases: 2,
  healthy: 2,
  degraded: 0,
  total_findings: 0,
  total_critical: 0,
  total_warning: 0,
  incidents_critical: 0,
  incidents_warning: 0,
  incidents_unknown: 0,
  stale_collection: 0,
  freshness_unknown: 0,
}

const DATABASES = [
  { name: 'prod', status: { collector_last_run: '2026-11-08T12:00:00Z' } },
  { name: 'analytics', status: { collector_last_run: '2026-11-08T12:05:00Z' } },
]

function fleetWith(summary) {
  return {
    data: { summary, databases: DATABASES },
    loading: false, error: null, refetch: vi.fn(),
  }
}

function renderHero(overrides) {
  return render(<FleetOverview database="all" onSelectDB={vi.fn()}
    fleet={fleetWith({ ...CLEAR, ...overrides })} />)
}

function expectNotHealthy(hero) {
  expect(hero).not.toHaveTextContent(/healthy/i)
  expect(hero).not.toHaveTextContent(/No open critical or warning issues/)
  expect(hero).not.toHaveTextContent(/No issues detected/)
}

describe('HealthHero', () => {
  it('claims no open issues only when every count is zero', () => {
    renderHero({})
    const hero = screen.getByTestId('health-hero')
    expect(hero).toHaveTextContent('No open critical or warning issues')
    expect(hero).toHaveTextContent(/last collection \d{2}:\d{2}/)
    expect(hero).not.toHaveTextContent(/All Systems Healthy/)
    expect(screen.queryByTestId('health-hero-cases-link')).toBeNull()
  })

  it('one critical incident with no critical findings links to Cases', () => {
    renderHero({ incidents_critical: 1 })
    const hero = screen.getByTestId('health-hero')
    expectNotHealthy(hero)
    expect(hero).toHaveTextContent('1 critical incident')
    expect(screen.getByTestId('health-hero-cases-link'))
      .toHaveAttribute('href', '#/cases')
  })

  it('moves from the clear state to open work when an incident opens', () => {
    const { rerender } = renderHero({})
    expect(screen.getByTestId('health-hero'))
      .toHaveTextContent('No open critical or warning issues')
    rerender(<FleetOverview database="all" onSelectDB={vi.fn()}
      fleet={fleetWith({ ...CLEAR, incidents_critical: 1 })} />)
    expectNotHealthy(screen.getByTestId('health-hero'))
    expect(screen.getByTestId('health-hero-cases-link')).toBeInTheDocument()
  })

  it('one warning finding blocks the clear state', () => {
    renderHero({ total_warning: 1 })
    const hero = screen.getByTestId('health-hero')
    expectNotHealthy(hero)
    expect(hero).toHaveTextContent('1 warning finding')
    expect(hero).toHaveTextContent('1 Issue Needs Attention')
  })

  it('one warning incident blocks the clear state', () => {
    renderHero({ incidents_warning: 2 })
    const hero = screen.getByTestId('health-hero')
    expectNotHealthy(hero)
    expect(hero).toHaveTextContent('2 warning incidents')
  })

  it('lists critical work before warnings', () => {
    renderHero({ total_warning: 3, incidents_critical: 1, total_critical: 2 })
    const text = screen.getByTestId('health-hero').textContent
    expect(text.indexOf('1 critical incident'))
      .toBeLessThan(text.indexOf('3 warning findings'))
    expect(text.indexOf('2 critical findings'))
      .toBeLessThan(text.indexOf('3 warning findings'))
    expect(text).toContain('6 Issues Need Attention')
  })

  it('stale collection with zero findings shows the evidence gap', () => {
    renderHero({ stale_collection: 1 })
    const hero = screen.getByTestId('health-hero')
    expectNotHealthy(hero)
    expect(screen.getByTestId('health-hero-evidence-gap'))
      .toHaveTextContent('Collection is stale on 1 database')
    expect(hero).toHaveTextContent(/Health not established/)
  })

  it('unknown freshness with zero findings shows the evidence gap', () => {
    renderHero({ freshness_unknown: 2 })
    expectNotHealthy(screen.getByTestId('health-hero'))
    expect(screen.getByTestId('health-hero-evidence-gap'))
      .toHaveTextContent('Collection freshness is unknown on 2 databases')
  })

  it('unreadable incident counts block the clear state', () => {
    renderHero({ incidents_unknown: 1 })
    expectNotHealthy(screen.getByTestId('health-hero'))
    expect(screen.getByTestId('health-hero-evidence-gap'))
      .toHaveTextContent('Incident counts could not be read on 1 database')
  })

  it('a summary without the case counts is not read as zero', () => {
    renderHero({
      total_warning: undefined, incidents_critical: undefined,
      incidents_warning: undefined, incidents_unknown: undefined,
      stale_collection: undefined, freshness_unknown: undefined,
    })
    expectNotHealthy(screen.getByTestId('health-hero'))
    expect(screen.getByTestId('health-hero-evidence-gap'))
      .toHaveTextContent(/counts are unavailable/i)
  })

  it('a degraded database still blocks the clear state', () => {
    renderHero({ degraded: 1, healthy: 1 })
    const hero = screen.getByTestId('health-hero')
    expectNotHealthy(hero)
    expect(hero).toHaveTextContent('1 degraded database')
  })

  it('keeps the stat cards on findings and databases', () => {
    renderHero({ total_critical: 2, incidents_critical: 5 })
    expect(screen.getByText('Critical Issues').parentElement)
      .toHaveTextContent('2')
    expect(screen.getByTestId('stat-databases')).toHaveTextContent('2')
  })
})
