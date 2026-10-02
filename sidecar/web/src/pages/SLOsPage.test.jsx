import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SLOsPage } from './SLOsPage'

// Sage SRE M5: the SLO view shows each SLO's error-budget state, burn
// rates per window and why a state is unknown; app SLIs claim customer
// impact, database proxies are labeled as proxies. With one database
// selected it also shows that database's change feed.

const burn = (w, v, unknown) => ({ window: w, burn_rate: v, unknown })
const pageSLO = {
  name: 'checkout', database: 'orders', kind: 'app', source: 'prometheus',
  description: 'checkout availability', target: 0.999, window: '30d',
  state: 'page', fast_burning: true, customer_impact: true, unknown: [],
  budget_remaining: 0.25, evaluated_at: '2026-10-01T12:00:00Z',
  rules: [
    { severity: 'page', factor: 14.4, firing: true,
      long: burn('1h', 16.25), short: burn('5m', 15) },
    { severity: 'page', factor: 6, firing: false,
      long: burn('6h', 3.1), short: burn('30m', 2) },
    { severity: 'ticket', factor: 1, firing: false,
      long: burn('3d', null, 'partial_window'), short: burn('6h', 3.1) },
  ],
}
const unknownProxy = {
  name: 'db_latency', database: 'orders', kind: 'proxy', source: 'proxy',
  target: 0.99, window: '30d', state: 'unknown', fast_burning: false,
  customer_impact: false, unknown: ['baseline_building'], budget_remaining: null,
  evaluated_at: '2026-10-01T12:00:00Z',
  rules: [{ severity: 'page', factor: 14.4, firing: false,
    long: burn('1h', null, 'baseline_building'),
    short: burn('5m', null, 'baseline_building') }],
}
const change = {
  id: 'c1', kind: 'deploy', source: 'github-actions', summary: 'deploy checkout v1.2.3',
  occurred_at: '2026-10-01T11:58:00Z', signature: 'verified', database_scoped: true,
}

let responses
const urls = []

vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    const hit = Object.keys(responses).find(prefix => url?.startsWith(prefix))
    const r = hit ? responses[hit] : { data: null }
    return { data: r.data, loading: false, error: r.error || null, refetch: vi.fn() }
  },
}))

beforeEach(() => {
  urls.length = 0
  responses = {
    '/api/v1/sre/slos': { data: { slos: [pageSLO, unknownProxy], unavailable: [] } },
    '/api/v1/sre/changes': { data: { changes: [change] } },
  }
})

describe('SLOs page', () => {
  it('shows state, burn rates, budget and customer impact of an app SLI', () => {
    render(<SLOsPage database="all" />)
    expect(urls).toContain('/api/v1/sre/slos')
    const row = screen.getByText('checkout').closest('tr')
    expect(within(row).getByTestId('slo-state')).toHaveTextContent('Page')
    expect(within(row).getByTestId('slo-impact')).toHaveTextContent('Customer impact')
    expect(within(row).getByText(/16\.25x/)).toBeInTheDocument()
    expect(within(row).getByText(/15x/)).toBeInTheDocument()
    expect(within(row).getByTestId('slo-budget')).toHaveTextContent('25%')
    expect(within(row).getByText('App SLI')).toBeInTheDocument()
  })

  it('labels proxies and says why a state is unknown', () => {
    render(<SLOsPage database="all" />)
    const row = screen.getByText('db_latency').closest('tr')
    expect(within(row).getByTestId('slo-state')).toHaveTextContent('Unknown')
    expect(within(row).getByText('DB proxy')).toBeInTheDocument()
    expect(within(row).getByTestId('slo-unknown')).toHaveTextContent('baseline building')
    expect(within(row).queryByTestId('slo-impact')).toBeNull()
    expect(within(row).getByTestId('slo-budget')).toHaveTextContent('unknown')
  })

  it('shows the change feed only for one selected database', () => {
    const { unmount } = render(<SLOsPage database="all" />)
    expect(screen.queryByTestId('change-feed')).toBeNull()
    expect(urls.some(u => u?.startsWith('/api/v1/sre/changes'))).toBe(false)
    unmount()
    render(<SLOsPage database="orders" />)
    expect(urls).toContain('/api/v1/sre/slos?database=orders')
    expect(urls).toContain('/api/v1/sre/changes?database=orders&window_minutes=1440')
    const feed = screen.getByTestId('change-feed')
    expect(within(feed).getByText('deploy checkout v1.2.3')).toBeInTheDocument()
    expect(within(feed).getByText(/verified/)).toBeInTheDocument()
  })

  it('shows an empty state without SLOs and an error banner on failure', () => {
    responses['/api/v1/sre/slos'] = { data: { slos: [], unavailable: ['billing'] } }
    const { unmount } = render(<SLOsPage database="all" />)
    expect(screen.getByTestId('slo-empty')).toBeInTheDocument()
    expect(screen.getByText(/billing/)).toBeInTheDocument()
    unmount()
    responses['/api/v1/sre/slos'] = { data: null, error: '503 Service Unavailable' }
    render(<SLOsPage database="all" />)
    expect(screen.getByText(/503/)).toBeInTheDocument()
  })
})
