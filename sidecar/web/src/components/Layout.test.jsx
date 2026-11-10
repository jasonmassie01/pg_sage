import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { useAPI } from '../hooks/useAPI'
import { Layout } from './Layout'

// dashboard-12: App owns the one /api/v1/databases poll. Layout renders the
// fleet it is given and never polls the fleet itself, not even while App's
// first load is still running (fleetData null).
// No concurrent access tests: one render tree, single-threaded.

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(() => ({ data: null, refetch: vi.fn(), updatedAt: null })),
}))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

const operator = { email: 'op@example.com', role: 'operator' }
const databases = [{ name: 'orders', emergency_stopped: false, status: {} }]
const fleetData = { summary: { emergency_stopped: false }, databases }

function renderLayout(props = {}) {
  return render(
    <LiveEventsProvider>
      <Layout user={operator} databases={databases} selectedDB="orders"
        onSelectDB={() => {}} fleetData={fleetData}
        onFleetChanged={() => {}} {...props}>
        <p>page</p>
      </Layout>
    </LiveEventsProvider>,
  )
}

const requestedURLs = () => useAPI.mock.calls.map(([url]) => url)

beforeEach(() => {
  useAPI.mockClear()
  window.location.hash = '#/'
})
afterEach(() => {
  vi.unstubAllGlobals()
  window.location.hash = ''
})

describe('Layout fleet data', () => {
  it('makes no /api/v1/databases request when fleetData is given', () => {
    renderLayout()
    expect(screen.getByText('page')).toBeInTheDocument()
    // dashboard-02: the badge is scoped to the selected database.
    expect(requestedURLs()).toContain('/api/v1/actions/pending/count?database=orders')
    expect(requestedURLs()).not.toContain('/api/v1/databases')
  })

  it('makes no /api/v1/databases request while App is still loading', () => {
    renderLayout({ fleetData: null })
    expect(screen.getByText('page')).toBeInTheDocument()
    expect(useAPI).toHaveBeenCalled()
    expect(requestedURLs()).not.toContain('/api/v1/databases')
  })

  it('asks App to refetch the fleet after a confirmed stop', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200 }))
    vi.stubGlobal('fetch', fetchMock)
    const onFleetChanged = vi.fn()
    renderLayout({ onFleetChanged })
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    await act(async () => {
      fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
    })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/emergency-stop?database=orders',
      expect.objectContaining({ method: 'POST' }))
    expect(onFleetChanged).toHaveBeenCalledTimes(1)
  })

  it('still asks App to refetch when the stop request fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => {
      throw new TypeError('network down')
    }))
    const onFleetChanged = vi.fn()
    renderLayout({ onFleetChanged })
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    await act(async () => {
      fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
    })
    expect(screen.getByText('network down')).toHaveAttribute('role', 'alert')
    expect(onFleetChanged).toHaveBeenCalledTimes(1)
  })
})

// dashboard-02: the header says what pg_sage may do right now.
const fam = (action_type, decision) => ({ action_type, decision })
const fleetRow = (name, trust_level, families, extra = {}) => ({
  name, emergency_stopped: false,
  status: {
    trust_level,
    capabilities: families === undefined ? {} : { action_families: families },
  },
  ...extra,
})
const fleetOf = (rows, summary = {}) => ({
  summary: { emergency_stopped: false, ...summary }, databases: rows,
})
const pendingData = data => url => ({
  data: url?.startsWith('/api/v1/actions/pending/count') ? data : null,
  refetch: vi.fn(), updatedAt: null,
})
const idleAPI = () => ({ data: null, refetch: vi.fn(), updatedAt: null })

function rerenderLayout(rerender, props) {
  rerender(
    <LiveEventsProvider>
      <Layout user={operator} databases={databases} selectedDB="orders"
        onSelectDB={() => {}} fleetData={fleetData}
        onFleetChanged={() => {}} {...props}>
        <p>page</p>
      </Layout>
    </LiveEventsProvider>,
  )
}

describe('Layout header autonomy', () => {
  afterEach(() => { useAPI.mockImplementation(idleAPI) })

  it('shows Mixed for a mixed fleet and names both autonomous databases', () => {
    const rows = [
      fleetRow('a', 'autonomous', [fam('create_index', 'execute')]),
      fleetRow('b', 'autonomous', [fam('create_index', 'execute')]),
      fleetRow('c', 'observation', [fam('create_index', 'blocked')]),
    ]
    renderLayout({ selectedDB: 'all', databases: rows, fleetData: fleetOf(rows) })
    const badge = screen.getByTestId('trust-badge-mixed')
    expect(badge).toHaveTextContent('Mixed · 2 autonomous · 1 observation')
    expect(badge).toHaveAttribute('title', 'Auto-executes now: create_index on a, b')
    expect(screen.queryByTestId('trust-badge-observation')).toBeNull()
  })

  it('shows a plain Observation badge for an all-observation fleet', () => {
    const rows = [fleetRow('a', 'observation', []), fleetRow('b', 'observation', [])]
    renderLayout({ selectedDB: 'all', databases: rows, fleetData: fleetOf(rows) })
    expect(screen.getByTestId('trust-badge-observation')).toHaveTextContent('Observation')
    expect(screen.queryByTestId('trust-badge-mixed')).toBeNull()
  })

  it('lists per-database families on a one-level autonomous fleet', () => {
    const rows = [
      fleetRow('a', 'autonomous', [fam('analyze_table', 'execute')]),
      fleetRow('b', 'autonomous', [fam('analyze_table', 'queue')]),
    ]
    renderLayout({ selectedDB: 'all', databases: rows, fleetData: fleetOf(rows) })
    expect(screen.getByTestId('trust-badge-autonomous'))
      .toHaveAttribute('title', 'Auto-executes now: analyze_table on a')
  })

  it('uses the static hint when the selected database reports no families', () => {
    const rows = [fleetRow('orders', 'advisory')]
    renderLayout({ databases: rows, fleetData: fleetOf(rows) })
    expect(screen.getByTestId('trust-badge-advisory').getAttribute('title'))
      .toMatch(/execution_mode=auto/)
  })

  it('says nothing auto-executes when the selected database reports []', () => {
    const rows = [fleetRow('orders', 'autonomous', [])]
    renderLayout({ databases: rows, fleetData: fleetOf(rows) })
    expect(screen.getByTestId('trust-badge-autonomous').getAttribute('title'))
      .toMatch(/^Nothing auto-executes right now/)
  })

  it('lists exactly the one execute family of an advisory database', () => {
    const rows = [fleetRow('orders', 'advisory', [
      fam('analyze_table', 'execute'), fam('create_index', 'queue_for_approval'),
    ])]
    renderLayout({ databases: rows, fleetData: fleetOf(rows) })
    expect(screen.getByTestId('trust-badge-advisory'))
      .toHaveAttribute('title', 'Auto-executes now: analyze_table')
  })

  it('shows no execute families for a database after its emergency stop', () => {
    const before = [fleetRow('orders', 'autonomous', [fam('create_index', 'execute')])]
    const { rerender } = renderLayout({ databases: before, fleetData: fleetOf(before) })
    expect(screen.getByTestId('trust-badge-autonomous'))
      .toHaveAttribute('title', 'Auto-executes now: create_index')
    const after = [fleetRow('orders', 'autonomous', [fam('create_index', 'blocked')],
      { emergency_stopped: true })]
    rerenderLayout(rerender, {
      databases: after, fleetData: fleetOf(after, { emergency_stopped: true }),
    })
    expect(screen.getByTestId('trust-badge-autonomous').getAttribute('title'))
      .toMatch(/^Nothing auto-executes right now/)
  })

  it('counts an unknown trust level as unknown, never autonomous', () => {
    const rows = [fleetRow('a', 'autonomous', []), fleetRow('b', 'root', [])]
    renderLayout({ selectedDB: 'all', databases: rows, fleetData: fleetOf(rows) })
    expect(screen.getByTestId('trust-badge-mixed'))
      .toHaveTextContent('Mixed · 1 autonomous · 1 unknown')
  })
})

describe('Layout pending badge', () => {
  afterEach(() => { useAPI.mockImplementation(idleAPI) })

  it('shows N+ and names the databases it could not count', () => {
    useAPI.mockImplementation(pendingData({
      count: 3, errors: [{ database: 'b', error: 'not connected' }],
    }))
    renderLayout({ selectedDB: 'all' })
    const badge = screen.getByText('3+')
    expect(badge.getAttribute('title')).toMatch(/\bb\b/)
    expect(badge.getAttribute('title')).toMatch(/not connected/)
  })

  it('shows an exact count when every database answered', () => {
    useAPI.mockImplementation(pendingData({ count: 3, errors: [] }))
    renderLayout({ selectedDB: 'all' })
    expect(screen.getByText('3')).toBeInTheDocument()
    expect(screen.queryByText('3+')).toBeNull()
  })

  it('shows 0+ when nothing was counted because a database failed', () => {
    useAPI.mockImplementation(pendingData({
      count: 0, errors: [{ database: 'b', error: 'count pending actions failed' }],
    }))
    renderLayout({ selectedDB: 'all' })
    expect(screen.getByText('0+')).toBeInTheDocument()
  })

  it('scopes the count to the selected database', () => {
    const { rerender } = renderLayout({ selectedDB: 'all' })
    expect(requestedURLs()).toContain('/api/v1/actions/pending/count')
    rerenderLayout(rerender, { selectedDB: 'a' })
    // The last pending-count request: the secrets banner (security-08) also
    // calls useAPI, after the badge.
    const counts = requestedURLs().filter(u => u?.startsWith('/api/v1/actions/pending/count'))
    expect(counts.at(-1)).toBe('/api/v1/actions/pending/count?database=a')
  })

  it('does not poll the count for a viewer', () => {
    renderLayout({ user: { email: 'v@example.com', role: 'viewer' } })
    expect(requestedURLs().filter(Boolean)).toEqual([])
  })
})
