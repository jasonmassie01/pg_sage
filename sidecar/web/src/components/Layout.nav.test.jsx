import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { Layout } from './Layout'
import { findRoute } from '../routes'

// dashboard-06, ux-11: the nav is derived from the route registry: 14
// entries in Operate, Admin and a collapsible Evidence group whose open
// state the viewer's browser remembers.
// No concurrent access tests: one render tree, single-threaded.

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(() => ({ data: null, refetch: vi.fn(), updatedAt: null })),
}))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

const admin = { email: 'a@example.com', role: 'admin' }
const operator = { email: 'op@example.com', role: 'operator' }
const viewer = { email: 'v@example.com', role: 'viewer' }
const databases = [{ name: 'orders', emergency_stopped: false, status: {} }]
const fleetData = { summary: { emergency_stopped: false }, databases }
const ADMIN_LINKS = { 'nav-databases': '#/manage-databases', 'nav-autonomy': '#/autonomy',
  'nav-notifications': '#/notifications', 'nav-agents': '#/agents', 'nav-users': '#/users',
  'nav-settings': '#/settings' }
const EVIDENCE = ['nav-queries', 'nav-slos', 'nav-facts', 'nav-runbooks']

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

const sidebar = () => within(screen.getByTestId('sidebar-desktop'))
const evidenceToggle = () => sidebar().getByRole('button', { name: 'Evidence' })
const navIds = () => [...screen.getByTestId('sidebar-desktop')
  .querySelectorAll('a[data-testid^="nav-"]')].map(el => el.getAttribute('data-testid'))

beforeEach(() => {
  localStorage.clear()
  window.location.hash = '#/'
})
afterEach(() => {
  vi.restoreAllMocks()
  window.location.hash = ''
})

describe('Layout navigation groups', () => {
  it('puts Inbox, Changes, Issues and Ask under Operate', () => {
    renderLayout({ user: admin })
    expect(navIds().slice(0, 4)).toEqual(['nav-inbox', 'nav-actions', 'nav-cases', 'nav-ask'])
    expect(screen.getByTestId('nav-actions')).toHaveTextContent('Changes')
    expect(screen.getByTestId('nav-actions')).toHaveAttribute('href', '#/changes')
    expect(screen.getByTestId('nav-cases')).toHaveTextContent('Issues')
    expect(screen.getByTestId('nav-cases')).toHaveAttribute('href', '#/issues')
  })

  it('shows admins the six Admin entries in order, Fleet first', () => {
    renderLayout({ user: admin })
    expect(sidebar().getByText('Admin', { exact: true })).toBeInTheDocument()
    for (const [testId, href] of Object.entries(ADMIN_LINKS)) {
      expect(screen.getByTestId(testId)).toHaveAttribute('href', href)
    }
    expect(navIds().slice(4)).toEqual(Object.keys(ADMIN_LINKS))
    expect(screen.getByTestId('nav-databases')).toHaveTextContent('Fleet')
    for (const gone of ['nav-alerts', 'nav-mcp-tokens', 'nav-snapshot', 'nav-ledger']) {
      expect(screen.queryByTestId(gone), gone).toBeNull()
    }
  })

  it('shows a viewer only Autonomy under Admin and no empty group', () => {
    renderLayout({ user: viewer })
    for (const testId of Object.keys(ADMIN_LINKS).filter(id => id !== 'nav-autonomy')) {
      expect(screen.queryByTestId(testId), testId).toBeNull()
    }
    expect(screen.getByTestId('nav-autonomy')).toBeInTheDocument()
    for (const heading of ['Operate', 'Admin']) {
      expect(sidebar().getByText(heading, { exact: true })).toBeInTheDocument()
    }
    fireEvent.click(evidenceToggle())
    expect(navIds()).toHaveLength(9)
  })

  it('shows an operator Autonomy, Notifications and Agents under Admin', () => {
    renderLayout()
    expect(navIds().slice(4)).toEqual(['nav-autonomy', 'nav-notifications', 'nav-agents'])
    expect(screen.queryByTestId('nav-users')).toBeNull()
    expect(screen.queryByTestId('nav-settings')).toBeNull()
  })

  it('hides Agents when the platform manages sign-in', () => {
    renderLayout({ user: admin, authMode: 'databricks_app' })
    expect(screen.queryByTestId('nav-agents')).toBeNull()
    expect(screen.getByTestId('nav-users')).toBeInTheDocument()
  })

  it('highlights Issues for the old findings explorer URL', () => {
    window.location.hash = '#/advanced/findings'
    renderLayout({ user: admin })
    expect(screen.getByTestId('nav-cases')).toHaveAttribute('aria-current', 'page')
    expect(screen.getByTestId('nav-inbox')).not.toHaveAttribute('aria-current')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Issues')
  })

  it('links only to registered routes, 14 for an admin', () => {
    renderLayout({ user: admin })
    fireEvent.click(evidenceToggle())
    const hrefs = [...screen.getByTestId('sidebar-desktop')
      .querySelectorAll('a[data-testid^="nav-"]')].map(link => link.getAttribute('href'))
    expect(hrefs).toHaveLength(14)
    for (const href of hrefs) expect(findRoute(href), href).toBeDefined()
  })

  it('renders no nav links without a user', () => {
    renderLayout({ user: null })
    expect(screen.getByTestId('sidebar-desktop').querySelectorAll('[data-testid^="nav-"]'))
      .toHaveLength(0)
  })
})

describe('Layout Evidence group', () => {
  it('is collapsed by default and opens on a click', () => {
    renderLayout({ user: admin })
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'false')
    for (const id of EVIDENCE) expect(screen.queryByTestId(id), id).toBeNull()
    fireEvent.click(evidenceToggle())
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'true')
    expect(navIds().slice(10)).toEqual(EVIDENCE)
    expect(screen.getByTestId('nav-runbooks')).toHaveAttribute('href', '#/advanced/runbooks')
  })

  it('remembers the opened group for the next render', () => {
    const first = renderLayout({ user: admin })
    fireEvent.click(evidenceToggle())
    expect(localStorage.getItem('pg_sage_nav_evidence')).toBe('open')
    first.unmount()
    renderLayout({ user: admin })
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByTestId('nav-slos')).toBeInTheDocument()
  })

  it('opens on one of its pages until the viewer closes it', () => {
    window.location.hash = '#/slos'
    const first = renderLayout({ user: admin })
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByTestId('nav-slos')).toHaveAttribute('aria-current', 'page')
    fireEvent.click(evidenceToggle())
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'false')
    expect(localStorage.getItem('pg_sage_nav_evidence')).toBe('closed')
    first.unmount()
    renderLayout({ user: admin })
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'false')
  })

  it('still toggles when storage is blocked', () => {
    vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError')
    })
    renderLayout({ user: admin })
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'false')
    fireEvent.click(evidenceToggle())
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByTestId('nav-facts')).toBeInTheDocument()
  })

  it('ignores a stored value it did not write', () => {
    localStorage.setItem('pg_sage_nav_evidence', 'sideways')
    renderLayout({ user: admin })
    expect(evidenceToggle()).toHaveAttribute('aria-expanded', 'false')
  })
})
