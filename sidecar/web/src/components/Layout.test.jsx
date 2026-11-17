import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { useAPI } from '../hooks/useAPI'
import { Layout } from './Layout'
import { findRoute } from '../routes'

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
    expect(requestedURLs()).toContain('/api/v1/inbox/count?database=orders')
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

// The nav badge tests moved to Layout.badge.test.jsx with the badge itself
// (ux-01b: one badge, on Inbox, from /api/v1/inbox/count).

// dashboard-06: the nav is derived from the route registry.
describe('Layout navigation', () => {
  const admin = { email: 'a@example.com', role: 'admin' }
  const viewer = { email: 'v@example.com', role: 'viewer' }
  const ADMIN_LINKS = { 'nav-users': '#/users', 'nav-notifications': '#/notifications',
    'nav-alerts': '#/alerts' }
  const sidebar = () => within(screen.getByTestId('sidebar-desktop'))
  const openAdvanced = () =>
    fireEvent.click(sidebar().getByRole('button', { name: /advanced/i }))

  it('shows admins Users, Notifications and Alert log under Admin', () => {
    renderLayout({ user: admin })
    expect(sidebar().getByText('Admin', { exact: true })).toBeInTheDocument()
    for (const [testId, href] of Object.entries(ADMIN_LINKS)) {
      expect(screen.getByTestId(testId)).toHaveAttribute('href', href)
    }
    expect(screen.getByTestId('nav-alerts')).toHaveTextContent('Alert log')
    expect(screen.getByTestId('nav-databases')).toHaveTextContent('Fleet')
  })

  it('shows a viewer no admin link and no Admin heading', () => {
    renderLayout({ user: viewer })
    for (const testId of [...Object.keys(ADMIN_LINKS), 'nav-databases', 'nav-settings',
      'nav-mcp-tokens', 'nav-agents']) {
      expect(screen.queryByTestId(testId), testId).toBeNull()
    }
    expect(sidebar().queryByText('Admin', { exact: true })).toBeNull()
    expect(screen.getByTestId('nav-cases')).toBeInTheDocument()
  })

  it('shows an operator only Alert log and Agents under Admin', () => {
    renderLayout()
    expect(screen.getByTestId('nav-alerts')).toBeInTheDocument()
    expect(screen.getByTestId('nav-agents')).toBeInTheDocument()
    expect(screen.queryByTestId('nav-users')).toBeNull()
    expect(screen.queryByTestId('nav-settings')).toBeNull()
  })

  it('hides MCP tokens and Agents when the platform manages sign-in', () => {
    renderLayout({ user: admin, authMode: 'databricks_app' })
    expect(screen.queryByTestId('nav-mcp-tokens')).toBeNull()
    expect(screen.queryByTestId('nav-agents')).toBeNull()
    expect(screen.getByTestId('nav-users')).toBeInTheDocument()
  })

  it('highlights Cases for the old findings explorer URL', () => {
    window.location.hash = '#/advanced/findings'
    renderLayout({ user: admin })
    expect(screen.getByTestId('nav-cases')).toHaveAttribute('aria-current', 'page')
    expect(screen.getByTestId('nav-inbox')).not.toHaveAttribute('aria-current')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Cases')
  })

  it('lists no duplicate Cases or Actions entry under Advanced', () => {
    renderLayout({ user: admin })
    openAdvanced()
    expect(screen.queryByRole('link', { name: /findings explorer/i })).toBeNull()
    expect(screen.queryByRole('link', { name: /action history/i })).toBeNull()
    expect(screen.getByTestId('nav-runbooks')).toHaveAttribute('href', '#/advanced/runbooks')
    expect(screen.getByTestId('nav-snapshot')).toHaveAttribute('href', '#/advanced')
  })

  it('links only to registered routes', () => {
    renderLayout({ user: admin })
    openAdvanced()
    const hrefs = [...screen.getByTestId('sidebar-desktop').querySelectorAll('a[href^="#/"]')]
      .map(link => link.getAttribute('href'))
    expect(hrefs.length).toBeGreaterThan(15)
    for (const href of hrefs) expect(findRoute(href), href).toBeDefined()
  })

  it('renders no nav links without a user', () => {
    renderLayout({ user: null })
    expect(screen.getByTestId('sidebar-desktop').querySelectorAll('[data-testid^="nav-"]'))
      .toHaveLength(0)
  })
})
