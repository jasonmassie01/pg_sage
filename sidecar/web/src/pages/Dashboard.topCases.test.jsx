import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Dashboard } from './Dashboard'
import { useLiveRefetch } from '../hooks/useLiveEvents'

// cases-11b: the Overview's "Top issues" tab shows the five highest-ranked
// open issues, in the server's rank order, each linking to its case; it
// replaces the "Recent Recommendations" list of newest findings.
// No concurrent access tests: one render, single-threaded.

vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/FleetHealthChart', () => ({ FleetHealthChart: () => <div /> }))
vi.mock('../components/ProviderReadinessMatrix', () => ({
  ProviderReadinessMatrix: () => <div />,
}))
vi.mock('../components/TokenBudgetBanner', () => ({ TokenBudgetBanner: () => <div /> }))

const row = (id, db, severity, title, basis) => ({
  id: `finding:${db}:${id}`, database_name: db, severity, title,
  rank_basis: basis || `severity ${severity}; urgency not assessed; most recent first`,
  urgency: { assessed: false }, observed_at: '2026-11-10T12:00:00Z',
  source_type: 'finding', state: 'open',
})

const RANKED = [
  row('lock', 'beta', 'critical', 'Writers blocked by a lock chain',
    'severity critical; limit already reached'),
  row('disk', 'alpha', 'critical', 'Disk fills in about 3 hours',
    'severity critical; time to impact about 3h'),
  row('stats', 'alpha', 'warning', 'Stale statistics on orders'),
  row('bloat', 'beta', 'warning', 'Index bloat on events'),
  row('unused', 'alpha', 'info', 'Unused index on users'),
]

let casesQuery
let urls
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    if (url && url.startsWith('/api/v1/cases')) return casesQuery
    return { data: null, loading: false, error: null, refetch: vi.fn() }
  },
}))

const fleet = {
  data: {
    summary: { total_databases: 2, healthy: 2, degraded: 0, total_critical: 2 },
    databases: [{ name: 'alpha', status: 'healthy' }, { name: 'beta', status: 'healthy' }],
  },
  loading: false, error: null, refetch: vi.fn(),
}

const okCases = (data) => ({ data, loading: false, error: null, refetch: vi.fn() })

function openTopCases(database = 'all') {
  render(<Dashboard database={database} onSelectDB={vi.fn()} fleet={fleet} />)
  fireEvent.click(screen.getByTestId('overview-tab-top-cases'))
  return screen.getByTestId('top-cases')
}

const casesURLs = () => urls.filter(u => u && u.startsWith('/api/v1/cases'))

beforeEach(() => {
  urls = []
  vi.mocked(useLiveRefetch).mockClear()
  casesQuery = okCases({ cases: RANKED, total: 12, unavailable: [], truncated: {} })
})

describe('Dashboard top cases', () => {
  it('renders the five highest-ranked cases in rank order', () => {
    const panel = openTopCases()
    const rows = within(panel).getAllByTestId('top-case-row')
    expect(rows.map(r => r.textContent)).toEqual(RANKED.map(c =>
      expect.stringContaining(c.title)))
    expect(rows).toHaveLength(5)
    expect(rows[0]).toHaveTextContent('limit already reached')
    expect(rows[1]).toHaveTextContent('time to impact about 3h')
    expect(rows[0]).toHaveTextContent('beta')
  })

  it('shows at most five rows even when the server sends more', () => {
    casesQuery = okCases({ cases: [...RANKED, row('extra', 'alpha', 'info', 'Sixth case')],
      total: 6, unavailable: [] })
    const panel = openTopCases()
    expect(within(panel).getAllByTestId('top-case-row')).toHaveLength(5)
    expect(within(panel).queryByText('Sixth case')).toBeNull()
  })

  it('links each row to its case and database', () => {
    const panel = openTopCases()
    const links = within(panel).getAllByTestId('top-case-link')
    const first = new URLSearchParams(links[0].getAttribute('href').split('?')[1])
    expect(links[0].getAttribute('href')).toMatch(/^#\/cases\?/)
    expect(first.get('case')).toBe('finding:beta:lock')
    expect(first.get('database')).toBe('beta')
    const third = new URLSearchParams(links[2].getAttribute('href').split('?')[1])
    expect(third.get('case')).toBe('finding:alpha:stats')
    expect(third.get('database')).toBe('alpha')
  })

  it('requests the ranked summary only once the tab is open', () => {
    render(<Dashboard database="all" onSelectDB={vi.fn()} fleet={fleet} />)
    expect(casesURLs()).toEqual([])
    fireEvent.click(screen.getByTestId('overview-tab-top-cases'))
    const url = casesURLs().at(-1)
    const params = new URLSearchParams(url.split('?')[1])
    expect(url.split('?')[0]).toBe('/api/v1/cases')
    expect(params.get('summary')).toBe('1')
    expect(params.get('limit')).toBe('5')
    expect(params.has('database')).toBe(false)
    expect(urls.some(u => u && u.startsWith('/api/v1/findings'))).toBe(false)
  })

  it('scopes the request to the selected database', () => {
    openTopCases('prod db')
    const params = new URLSearchParams(casesURLs().at(-1).split('?')[1])
    expect(params.get('database')).toBe('prod db')
    expect(params.get('summary')).toBe('1')
  })

  it('refetches on findings and actions events for the selected database', () => {
    openTopCases('alpha')
    const calls = vi.mocked(useLiveRefetch).mock.calls
    const active = calls.filter(([, refetch]) => refetch === casesQuery.refetch)
    expect(active.length).toBeGreaterThan(0)
    expect(active.at(-1)[0]).toEqual(['findings', 'actions'])
    expect(active.at(-1)[2]).toEqual({ database: 'alpha' })
  })

  it('says how many cases are open and links to all of them', () => {
    const panel = openTopCases('alpha')
    expect(panel).toHaveTextContent('Top 5 of 12 open issues')
    const all = within(panel).getByTestId('top-cases-all')
    expect(all.getAttribute('href')).toBe('#/cases?database=alpha')
  })

  it('says the count is a floor when a database list was cut at its cap', () => {
    casesQuery = okCases({ cases: RANKED, total: 700, unavailable: [],
      truncated: { alpha: { findings: true, incidents: false, query_hints: false } } })
    const panel = openTopCases()
    expect(panel).toHaveTextContent('Top 5 of at least 700 open issues')
    expect(within(panel).getByTestId('cases-truncated')).toHaveTextContent('alpha')
    expect(within(panel).getByTestId('cases-truncated')).toHaveTextContent('findings')
  })

  it('reports an unreachable database instead of blanking the list', () => {
    casesQuery = okCases({ cases: RANKED.filter(c => c.database_name === 'alpha'),
      total: 3, unavailable: [{ database: 'beta', error_class: 'connection' }] })
    const panel = openTopCases()
    expect(within(panel).getAllByTestId('top-case-row')).toHaveLength(3)
    const note = within(panel).getByTestId('cases-unavailable')
    expect(note).toHaveTextContent('beta')
    expect(note).toHaveTextContent(/could not be reached|connection/i)
  })

  it('never claims no open issues when a database could not be read', () => {
    casesQuery = okCases({ cases: [], total: 0,
      unavailable: [{ database: 'beta', error_class: 'timeout' }] })
    const panel = openTopCases()
    expect(within(panel).queryByText(/No open issues/)).toBeNull()
    expect(within(panel).getByTestId('cases-unavailable')).toHaveTextContent('beta')
  })

  it('says no open issues only when every database answered with none', () => {
    casesQuery = okCases({ cases: [], total: 0, unavailable: [] })
    const panel = openTopCases()
    expect(panel).toHaveTextContent('No open issues in the selected scope.')
    expect(within(panel).queryAllByTestId('top-case-row')).toHaveLength(0)
  })

  it('shows the request error instead of an empty state', () => {
    casesQuery = { data: null, loading: false, error: '500 Internal Server Error',
      refetch: vi.fn() }
    const panel = openTopCases()
    expect(panel).toHaveTextContent('500 Internal Server Error')
    expect(within(panel).queryByText(/No open issues/)).toBeNull()
  })

  it('shows a loading state, not an empty state, before the first answer', () => {
    casesQuery = { data: null, loading: true, error: null, refetch: vi.fn() }
    const panel = openTopCases()
    expect(within(panel).getByTestId('top-cases-loading')).toBeInTheDocument()
    expect(within(panel).queryByText(/No open issues/)).toBeNull()
  })

  it('treats a malformed answer as an error, not as no cases', () => {
    casesQuery = okCases({ total: 3 })
    const panel = openTopCases()
    expect(within(panel).queryByText(/No open issues/)).toBeNull()
    expect(panel).toHaveTextContent(/could not be read/i)
  })

  it('describes the tab by rank, not by recency', () => {
    openTopCases()
    expect(screen.getByTestId('overview-tab-description'))
      .toHaveTextContent(/highest-ranked open issues/i)
    expect(screen.queryByTestId('overview-tab-recent-recos')).toBeNull()
    expect(screen.queryByText('Recent Recommendations')).toBeNull()
  })
})
