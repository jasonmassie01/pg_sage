import { fireEvent, render, screen, within } from '@testing-library/react'
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
// Each useAPI call as [url, interval]: an interval of 0 never polls.
const calls = []

vi.mock('../hooks/useAPI', () => ({
  useAPI: (url, interval) => {
    urls.push(url)
    calls.push([url, interval])
    const hit = Object.keys(responses).find(prefix => url?.startsWith(prefix))
    const r = hit ? responses[hit] : { data: null }
    return { data: r.data, loading: false, error: r.error || null, refetch: vi.fn() }
  },
}))

beforeEach(() => {
  urls.length = 0
  calls.length = 0
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

  it('badges a Prometheus SLO whose window coverage is unverified', () => {
    const unverified = { ...pageSLO, name: 'search', state: 'ok', fast_burning: false,
      customer_impact: false, coverage_unverified: true }
    responses['/api/v1/sre/slos'] = {
      data: { slos: [pageSLO, unverified, unknownProxy], unavailable: [] },
    }
    render(<SLOsPage database="all" />)
    const row = screen.getByText('search').closest('tr')
    expect(within(row).getByTestId('slo-coverage-unverified'))
      .toHaveTextContent('coverage unverified')
    const paged = screen.getByText('checkout').closest('tr')
    expect(within(paged).queryByTestId('slo-coverage-unverified')).toBeNull()
    const proxy = screen.getByText('db_latency').closest('tr')
    expect(within(proxy).queryByTestId('slo-coverage-unverified')).toBeNull()
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

// playbooks-09: each row says how long the SLO has been in its state and
// when its burn started; the budget names why it is unknown (with the
// covered share of a partial window); a row expands into the SLO's
// transitions, its recovery verdict and the newest slo:<name>
// investigation.

const minutesAgo = m => new Date(Date.now() - m * 60000).toISOString()
const burnStart = minutesAgo(40)
const burning = { ...pageSLO, state_since: burnStart, burn_started_at: burnStart }
const quiet = { ...unknownProxy, state_since: minutesAgo(3 * 1440 + 120) }
const detailURL = '/api/v1/sre/slos/checkout?database=orders&since=' +
  encodeURIComponent(burnStart)
const investigationsURL =
  '/api/v1/investigations?database=orders&case_id=slo%3Acheckout&limit=1'
const investigation = {
  id: 'inv-7', database: 'orders', case_id: 'slo:checkout', state: 'concluded',
  trigger_kind: 'slo_burn', summary: { family: 'slo_burn' },
}
const detail = {
  database: 'orders', status: burning,
  transitions: [
    { from: 'ok', to: 'page', at: burnStart },
    { to: 'ok', at: minutesAgo(5 * 1440) },
  ],
  recovery: { state: 'not_recovered', reason: 'burning', since: burnStart, evaluated: 3 },
}

// withResponses puts the more specific prefixes first: the mock answers
// a URL with the first prefix it starts with.
function withResponses(extra, slos = [burning, quiet]) {
  responses = {
    ...extra,
    '/api/v1/sre/slos': { data: { slos, unavailable: [] } },
    '/api/v1/sre/changes': { data: { changes: [] } },
  }
}

function openRow(name) {
  const row = screen.getByText(name).closest('tr')
  fireEvent.click(within(row).getByTestId('slo-expand'))
  return screen.getByTestId('slo-detail')
}

describe('SLOs page: since when, and who is investigating', () => {
  it('shows how long a paging SLO has burned and how long it has been in its state', () => {
    withResponses({})
    render(<SLOsPage database="all" />)
    const row = screen.getByText('checkout').closest('tr')
    expect(within(row).getByTestId('slo-state-age')).toHaveTextContent('in page for 40m')
    expect(within(row).getByTestId('slo-burn-start'))
      .toHaveTextContent('burning since 40m ago')
    const proxy = screen.getByText('db_latency').closest('tr')
    expect(within(proxy).getByTestId('slo-state-age'))
      .toHaveTextContent('in unknown for 3d 2h')
  })

  it('shows no burn line for an SLO that is not burning', () => {
    withResponses({})
    const { unmount } = render(<SLOsPage database="all" />)
    const proxy = screen.getByText('db_latency').closest('tr')
    expect(within(proxy).queryByTestId('slo-burn-start')).toBeNull()
    unmount()
    const noSince = { ...quiet, name: 'fresh', state_since: undefined }
    withResponses({}, [noSince])
    render(<SLOsPage database="all" />)
    const fresh = screen.getByText('fresh').closest('tr')
    expect(within(fresh).queryByTestId('slo-state-age')).toBeNull()
    expect(within(fresh).queryByTestId('slo-burn-start')).toBeNull()
  })

  it('expands into the transitions, the recovery verdict and the investigation', () => {
    withResponses({
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': { data: { items: [investigation] } },
    })
    render(<SLOsPage database="all" />)
    expect(screen.queryByTestId('slo-detail')).toBeNull()
    const panel = openRow('checkout')
    expect(urls).toContain(detailURL)
    expect(urls).toContain(investigationsURL)
    const steps = within(panel).getAllByTestId('slo-transition')
    expect(steps).toHaveLength(2)
    expect(steps[0]).toHaveTextContent('ok → page')
    expect(steps[0]).toHaveTextContent('40m ago')
    expect(steps[1]).toHaveTextContent('first evaluation → ok')
    expect(within(panel).getByTestId('slo-recovery'))
      .toHaveTextContent('Recovery: not recovered (burning)')
    const inv = within(panel).getByTestId('investigation-panel')
    expect(within(inv).getByTestId('investigation-state')).toHaveTextContent('Concluded')
    expect(within(inv).getByText('slo_burn')).toBeInTheDocument()
  })

  it('asks for no recovery verdict while the SLO is not burning', () => {
    withResponses({
      '/api/v1/sre/slos/db_latency': { data: { ...detail, recovery: undefined } },
      '/api/v1/investigations': { data: { items: [] } },
    })
    render(<SLOsPage database="all" />)
    const panel = openRow('db_latency')
    expect(urls).toContain('/api/v1/sre/slos/db_latency?database=orders')
    expect(urls.some(u => u?.includes('since='))).toBe(false)
    expect(within(panel).queryByTestId('slo-recovery')).toBeNull()
  })

  it('says why no investigation is shown when none was opened', () => {
    withResponses({
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': { data: { items: [] } },
    })
    render(<SLOsPage database="all" />)
    const panel = openRow('checkout')
    expect(within(panel).queryByTestId('investigation-panel')).toBeNull()
    expect(within(panel).getByTestId('slo-no-investigation')).toHaveTextContent(
      'No investigation opened (sre.slo.open_investigations or automatic_start off)')
  })

  it('shows the server message of a failed detail inside the expansion', () => {
    withResponses({
      '/api/v1/sre/slos/checkout': {
        data: null, error: '2 databases have this SLO: name the database',
      },
      '/api/v1/investigations': { data: { items: [] } },
    })
    render(<SLOsPage database="all" />)
    const panel = openRow('checkout')
    expect(within(panel).getByText(/name the database/)).toBeInTheDocument()
    expect(within(panel).queryAllByTestId('slo-transition')).toHaveLength(0)
    expect(screen.getByText('db_latency').closest('tr')).toBeInTheDocument()
    expect(screen.getAllByTestId('slo-state')).toHaveLength(2)
  })

  it('shows a failed investigation lookup without hiding the timeline', () => {
    withResponses({
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': { data: null, error: 'database not found' },
    })
    render(<SLOsPage database="all" />)
    const panel = openRow('checkout')
    expect(within(panel).getByText(/database not found/)).toBeInTheDocument()
    expect(within(panel).getAllByTestId('slo-transition')).toHaveLength(2)
    expect(within(panel).queryByTestId('slo-no-investigation')).toBeNull()
  })

  it('reads the detail once per open, not on each list poll', () => {
    const extra = {
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': { data: { items: [investigation] } },
    }
    withResponses(extra)
    const { rerender } = render(<SLOsPage database="all" />)
    openRow('checkout')
    withResponses(extra, [{ ...burning, evaluated_at: minutesAgo(0) }, quiet])
    rerender(<SLOsPage database="all" />)
    const listPolls = calls.filter(([u]) => u === '/api/v1/sre/slos')
    expect(listPolls.length).toBeGreaterThan(1)
    expect(listPolls.every(([, i]) => i === 30000)).toBe(true)
    const reads = calls.filter(([u]) => u?.startsWith('/api/v1/sre/slos/'))
    expect(reads.length).toBeGreaterThan(0)
    expect(reads.every(([u, i]) => u === detailURL && i === 0)).toBe(true)
    const invReads = calls.filter(([u]) => u?.startsWith('/api/v1/investigations'))
    expect(invReads.length).toBeGreaterThan(0)
    expect(invReads.every(([u, i]) => u === investigationsURL && i === 0)).toBe(true)
    fireEvent.click(within(screen.getByText('checkout').closest('tr'))
      .getByTestId('slo-expand'))
    expect(screen.queryByTestId('slo-detail')).toBeNull()
  })

  it('always names the database of the row it expands', () => {
    const other = { ...burning, database: 'billing' }
    withResponses({
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': { data: { items: [] } },
    }, [other])
    render(<SLOsPage database="all" />)
    openRow('checkout')
    expect(urls).toContain('/api/v1/sre/slos/checkout?database=billing&since=' +
      encodeURIComponent(burnStart))
    expect(urls).toContain(
      '/api/v1/investigations?database=billing&case_id=slo%3Acheckout&limit=1')
  })
})

// playbooks-09 review: a fleet SLO with an owner_database opens its
// investigation on the owner only, and an investigator that could not be
// read never reads as "no investigation opened".
describe('SLOs page: where the investigation lives', () => {
  it('looks up the investigation on the owner database of a fleet SLO', () => {
    const owned = { ...burning, database: 'orders', owner_database: 'billing' }
    withResponses({
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': {
        data: { items: [{ ...investigation, database: 'billing' }], unavailable: [] },
      },
    }, [owned])
    render(<SLOsPage database="all" />)
    const panel = openRow('checkout')
    expect(urls).toContain(detailURL)
    expect(urls).toContain(
      '/api/v1/investigations?database=billing&case_id=slo%3Acheckout&limit=1')
    expect(urls.some(u => u?.startsWith('/api/v1/investigations?database=orders')))
      .toBe(false)
    expect(within(panel).getByTestId('slo-owner'))
      .toHaveTextContent('Investigated on billing (owner_database)')
    expect(within(panel).getByTestId('investigation-panel')).toBeInTheDocument()
  })

  it('shows no owner line when the SLO is investigated on its own database', () => {
    withResponses({
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': { data: { items: [investigation], unavailable: [] } },
    })
    render(<SLOsPage database="all" />)
    const panel = openRow('checkout')
    expect(within(panel).queryByTestId('slo-owner')).toBeNull()
    expect(within(panel).getByTestId('investigation-panel')).toBeInTheDocument()
  })

  it('says an unreadable investigator is unknown, not that none was opened', () => {
    withResponses({
      '/api/v1/sre/slos/checkout': { data: detail },
      '/api/v1/investigations': { data: { items: [], unavailable: ['orders'] } },
    })
    render(<SLOsPage database="all" />)
    const panel = openRow('checkout')
    expect(within(panel).queryByTestId('slo-no-investigation')).toBeNull()
    expect(within(panel).getByTestId('slo-investigation-unavailable'))
      .toHaveTextContent('Investigations of orders unavailable')
    expect(within(panel).getAllByTestId('slo-transition')).toHaveLength(2)
  })
})

describe('SLOs page: why the budget is unknown', () => {
  const budgetOf = slo => {
    withResponses({}, [slo])
    render(<SLOsPage database="all" />)
    return screen.getByTestId('slo-budget')
  }

  it('names a partial window with its covered share, never the bare word unknown', () => {
    const el = budgetOf({ ...quiet, budget_remaining: null,
      budget_unknown: 'partial_window', budget_coverage: 0.1 })
    expect(el).toHaveTextContent('partial window: 10% covered')
    expect(el.textContent.trim()).not.toBe('unknown')
    expect(el).not.toHaveTextContent('spent so far')
  })

  it('adds the budget the covered part spent', () => {
    const el = budgetOf({ ...quiet, budget_remaining: null,
      budget_unknown: 'partial_window', budget_coverage: 0.899, budget_spent_so_far: 0.5 })
    // 89.9% covered is shown as 89%: a partial window never reads as 90%.
    expect(el).toHaveTextContent('partial window: 89% covered')
    expect(el).toHaveTextContent('spent so far: 50%')
  })

  it('names other unknown reasons and never shows an unknown budget as a number', () => {
    const el = budgetOf({ ...quiet, budget_remaining: null,
      budget_unknown: 'zero_eligible', budget_coverage: 1 })
    expect(el).toHaveTextContent('zero eligible')
    expect(el).not.toHaveTextContent('%')
  })

  it('says unknown with no reason when the status names none', () => {
    const el = budgetOf({ ...quiet, budget_remaining: null })
    expect(el).toHaveTextContent('unknown (no reason recorded)')
  })

  it('shows a known budget of zero as 0%, not unknown', () => {
    const el = budgetOf({ ...quiet, budget_remaining: 0 })
    expect(el).toHaveTextContent('0%')
    expect(el).not.toHaveTextContent('unknown')
  })
})
