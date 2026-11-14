import { render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { QueriesPage } from './QueriesPage'
import { QueryDetailPage } from './QueryDetailPage'
import { StackedAreaChart } from '../components/StackedAreaChart'
import { queryDetailParams, stackSeries } from '../lib/workload'
import { stubFetch } from '../test/fetchStub'
import { QueryPageLink } from './cases/QueryPageLink'

// market-01: the workload explorer. The top table ranks the window's
// statements by load, the chart stacks the sampled wait classes, and a
// query's page shows its history, plan changes, plan and links.
// No concurrent access tests: polled GETs and local state only.

afterEach(() => vi.unstubAllGlobals())

const topRow = (over = {}) => ({
  database: 'shop', queryid: '9007199254740993', query: 'SELECT * FROM orders WHERE id = $1',
  calls: 1200, total_time_ms: 36000, mean_time_ms: 30, rows: 1200, load: 0.01,
  intervals: 4, epoch_breaks: 0, ...over,
})

const waitBody = (over = {}) => ({
  database: 'shop', coverage: 'full', source: 'pg_stat_activity',
  sampling: 'sampled each collector cycle', classes: ['CPU', 'Lock'],
  series: [
    { t: '2026-11-10T10:00:00Z', sessions: 3, by_class: { CPU: 2, Lock: 1 } },
    { t: '2026-11-10T10:01:00Z', sessions: 1, by_class: { CPU: 1 } },
  ],
  events: [{ wait_event_type: 'CPU', wait_event: '', sessions: 3 }],
  ...over,
})

function renderQueries(top, wait = waitBody(), database = 'shop') {
  const fetch = stubFetch({
    '/api/v1/queries/top': { json: top },
    '/api/v1/wait-load': { json: wait },
  })
  render(<QueriesPage database={database} />)
  return fetch
}

describe('QueriesPage', () => {
  it('ranks the top statements with load and links each to its query page', async () => {
    renderQueries({ database: 'shop', coverage: 'full', order: 'load',
      queries: [topRow(), topRow({ queryid: '7', query: 'UPDATE t SET a = $1', load: 0.002 })] })
    const table = await screen.findByTestId('top-queries')
    const rows = within(table).getAllByTestId('top-query-row')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('SELECT * FROM orders')
    expect(rows[0]).toHaveTextContent('0.010')
    const link = within(rows[0]).getByRole('link', { name: /9007199254740993/ })
    expect(link).toHaveAttribute('href',
      '#/query?queryid=9007199254740993&database=shop')
  })

  it('says there is no data instead of showing zeros when coverage is none', async () => {
    renderQueries({ database: 'shop', coverage: 'none', order: 'load', queries: [] },
      waitBody({ coverage: 'none', series: [], events: [], classes: [] }))
    expect(await screen.findByTestId('top-queries-empty'))
      .toHaveTextContent(/no query samples/i)
    expect(screen.queryByTestId('top-query-row')).toBeNull()
  })

  it('keeps the series and shows the reason when the statement text is missing', async () => {
    renderQueries({ database: 'shop', coverage: 'full', order: 'load',
      text_unavailable: 'pg_stat_statements is not usable here',
      queries: [topRow({ query: '', text_unavailable: 'pg_stat_statements is not usable here' })] })
    const row = await screen.findByTestId('top-query-row')
    expect(row).toHaveTextContent('pg_stat_statements is not usable here')
    expect(row).toHaveTextContent('1,200')
  })

  it('labels the wait chart as sampled each collector cycle, not ASH', async () => {
    renderQueries({ database: 'shop', coverage: 'full', order: 'load', queries: [topRow()] })
    const chart = await screen.findByTestId('wait-load')
    expect(chart).toHaveTextContent('sampled each collector cycle')
    expect(chart).not.toHaveTextContent(/active session history/i)
    expect(within(chart).getAllByTestId(/^stack-/)).toHaveLength(2)
  })

  it('shows the provider breakdown as preferred when one is reported', async () => {
    renderQueries({ database: 'shop', coverage: 'full', order: 'load', queries: [] },
      waitBody({ preferred_source: 'performance_insights',
        provider: { source: 'performance_insights', load_by_wait: { CPU: 1.5, IO: 0.5 } } }))
    expect(await screen.findByTestId('wait-provider'))
      .toHaveTextContent(/performance insights/i)
  })

  it('names Lakebase wait counters as the preferred source (market-01b)', async () => {
    renderQueries({ database: 'shop', coverage: 'full', order: 'load', queries: [] },
      waitBody({ preferred_source: 'lakebase_wait_counters',
        provider: { source: 'lakebase', load_by_wait: { LWLock: 0.25, CPU: 1 },
          wait_event_time_unit: 'ms', unit_confirmed: false } }))
    const line = await screen.findByTestId('wait-provider')
    expect(line).toHaveTextContent(/Preferred source: Lakebase wait counters/)
    expect(line).toHaveTextContent('CPU 1.00, LWLock 0.25')
    expect(line).toHaveTextContent(/unit not yet confirmed/i)
  })

  it('names the database of each row in the fleet view and asks for one for waits', async () => {
    const fetch = renderQueries({ database: 'all', coverage: 'full', order: 'load',
      queries: [topRow({ database: 'a' }), topRow({ database: 'b', queryid: '8' })] },
    waitBody(), 'all')
    const rows = await screen.findAllByTestId('top-query-row')
    expect(rows[0]).toHaveTextContent('a')
    expect(rows[1]).toHaveTextContent('b')
    expect(screen.getByTestId('wait-load-pick')).toHaveTextContent(/select a database/i)
    expect(fetch.mock.calls.some(([u]) => String(u).startsWith('/api/v1/wait-load')))
      .toBe(false)
  })

  it('surfaces a failed top request', async () => {
    stubFetch({
      '/api/v1/queries/top': { status: 400, json: { error: 'limit must be 1..100' } },
      '/api/v1/wait-load': { json: waitBody() },
    })
    render(<QueriesPage database="shop" />)
    expect(await screen.findByText(/limit must be 1..100/)).toBeInTheDocument()
  })
})

describe('QueryDetailPage', () => {
  it('reads the queryid and database from the route', () => {
    expect(queryDetailParams('#/query?queryid=-42&database=shop', 'all'))
      .toEqual({ queryid: '-42', database: 'shop' })
    expect(queryDetailParams('#/query?queryid=5', 'main'))
      .toEqual({ queryid: '5', database: 'main' })
    expect(queryDetailParams('#/query?queryid=abc', 'main').queryid).toBeNull()
  })

  it('shows history, one plan change marker, the plan and the case links', async () => {
    stubFetch({ '/api/v1/queries/42': { json: {
      database: 'shop', queryid: '42', query: 'SELECT 1', coverage: 'full',
      buckets: [
        { start: '2026-11-10T10:00:00Z', calls: 10, mean_time_ms: 2, rows: 10 },
        { start: '2026-11-10T10:05:00Z', calls: 20, mean_time_ms: 9, rows: 20 },
      ],
      plan_changes: [{ at: '2026-11-10T10:05:00Z', from: 'aa', to: 'bb' }],
      plan: { source: 'auto_explain', total_cost: 42.5, node_types: ['Seq Scan'],
        plan_json: [{ Plan: { 'Node Type': 'Seq Scan' } }] },
      cases: [{ finding_id: 9, title: 'Slow query', status: 'open', severity: 'warning' }],
      hints: [{ id: 3, status: 'active', hint_text: 'SeqScan(t)' }],
      actions: [],
    } } })
    render(<QueryDetailPage route="#/query?queryid=42&database=shop" database="all" />)
    expect(await screen.findByTestId('query-history')).toBeInTheDocument()
    expect(screen.getAllByTestId('plan-change')).toHaveLength(1)
    expect(screen.getByTestId('query-plan')).toHaveTextContent('Seq Scan')
    const caseLink = screen.getByRole('link', { name: /Slow query/ })
    expect(caseLink.getAttribute('href')).toContain('9')
    expect(screen.getByTestId('query-hints')).toHaveTextContent('SeqScan(t)')
  })

  it('asks for a database when the scope is the fleet', () => {
    stubFetch({})
    render(<QueryDetailPage route="#/query?queryid=42" database="all" />)
    expect(screen.getByTestId('query-detail-pick')).toHaveTextContent(/select a database/i)
  })
})

describe('StackedAreaChart', () => {
  it('stacks each class on the ones below it', () => {
    const stacked = stackSeries([
      { by_class: { CPU: 2, Lock: 1 } }, { by_class: { Lock: 3 } },
    ], ['CPU', 'Lock'])
    expect(stacked).toEqual([
      { cls: 'CPU', points: [[0, 2], [0, 0]] },
      { cls: 'Lock', points: [[2, 3], [0, 3]] },
    ])
  })

  it('renders nothing to stack as an empty state, not an empty chart', () => {
    render(<StackedAreaChart series={[]} classes={[]} />)
    expect(screen.getByTestId('stacked-empty')).toBeInTheDocument()
  })
})

describe('QueryPageLink', () => {
  it('opens the query page of the evidence queryid in its database', () => {
    render(<QueryPageLink queryid="-42" database="shop" />)
    expect(screen.getByTestId('case-query-link'))
      .toHaveAttribute('href', '#/query?queryid=-42&database=shop')
  })

  it('renders nothing without a numeric queryid', () => {
    const { container } = render(<QueryPageLink queryid="abc" database="shop" />)
    expect(container).toBeEmptyDOMElement()
    render(<QueryPageLink database="shop" />)
    expect(screen.queryByTestId('case-query-link')).toBeNull()
  })
})
