import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { ROUTES } from './routes'
import { RENDERERS } from './routeRender'

// dashboard-06: App renders the page the route registry names. Old URLs
// stay as aliases, unrouted URLs render Not found, and the UI role gate
// still renders Access denied for a direct hash.
// No concurrent access tests: one render tree, single-threaded hash changes.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('./hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('./hooks/useLiveEvents', () => ({
  LiveEventsProvider: ({ children }) => children,
  useLiveRefetch: vi.fn(),
}))
vi.mock('./components/Toast', () => ({
  ToastProvider: ({ children }) => children,
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))
vi.mock('./pages/CasesPage', () => ({
  CasesPage: ({ initialSource }) => <div data-testid="cases-page">{initialSource || 'all'}</div>,
}))
vi.mock('./pages/Actions', () => ({ Actions: () => <div data-testid="actions-page" /> }))
vi.mock('./pages/UsersPage', () => ({ UsersPage: () => <div data-testid="users-page" /> }))
vi.mock('./pages/NotificationsPage', () => ({
  NotificationsPage: () => <div data-testid="notifications-page" />,
}))
vi.mock('./pages/ProfilePage', () => ({ ProfilePage: () => <div data-testid="profile-page" /> }))

const fleet = {
  summary: { emergency_stopped: false },
  databases: [{ id: 7, name: 'orders', status: { trust_level: 'advisory' } }],
}

function signIn(role) {
  globalThis.fetch = vi.fn(async url => ({
    ok: true, status: 200,
    json: async () => (String(url).startsWith('/api/v1/auth/me')
      ? { id: 1, email: `${role}@x.test`, role } : { mode: 'session' }),
  }))
}

async function open(hash, role = 'admin') {
  window.location.hash = hash
  signIn(role)
  render(<App />)
  await screen.findByTestId('app-loaded')
}
const title = () => screen.getByRole('heading', { level: 1 })

beforeEach(() => {
  localStorage.clear()
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => ({
    data: url === '/api/v1/databases' ? fleet : null,
    loading: false, error: null, refetch: vi.fn(),
  }))
})
afterEach(() => {
  window.location.hash = ''
  vi.restoreAllMocks()
})

describe('App routes from the registry', () => {
  it('has a renderer for every signed-in route and no other', () => {
    const signedIn = ROUTES.filter(route => !route.signedOutOnly).map(route => route.id)
    expect(Object.keys(RENDERERS).sort()).toEqual([...signedIn].sort())
  })

  it('opens the admin pages for an admin', async () => {
    await open('#/users')
    expect(screen.getByTestId('users-page')).toBeInTheDocument()
    expect(screen.getByTestId('nav-users')).toHaveAttribute('aria-current', 'page')
    await act(async () => {
      window.location.hash = '#/notifications'
      window.dispatchEvent(new HashChangeEvent('hashchange'))
    })
    expect(screen.getByTestId('notifications-page')).toBeInTheDocument()
    expect(title()).toHaveTextContent('Notifications')
  })

  it('still renders Access denied for a viewer on a direct admin hash', async () => {
    await open('#/users', 'viewer')
    expect(screen.getByTestId('access-denied')).toBeInTheDocument()
    expect(screen.queryByTestId('users-page')).toBeNull()
    expect(screen.queryByTestId('nav-users')).toBeNull()
    expect(screen.queryByTestId('nav-notifications')).toBeNull()
    expect(screen.queryByTestId('nav-alerts')).toBeNull()
  })

  it('sends operators from the alert log to the notification log', async () => {
    await open('#/alerts', 'operator')
    expect(await screen.findByTestId('notifications-page')).toBeInTheDocument()
    expect(title()).toHaveTextContent('Notifications')
  })

  it('denies a viewer the alert log on a direct hash', async () => {
    await open('#/alerts', 'viewer')
    expect(screen.getByTestId('access-denied')).toBeInTheDocument()
    expect(screen.queryByTestId('notifications-page')).toBeNull()
    expect(title()).toHaveTextContent('Access denied')
  })

  it('renders Cases for the old findings explorer URL', async () => {
    await open('#/advanced/findings')
    expect(screen.getByTestId('cases-page')).toHaveTextContent('all')
    expect(title()).toHaveTextContent('Issues')
    expect(screen.getByTestId('nav-cases')).toHaveAttribute('aria-current', 'page')
  })

  it('renders Actions for the old action history URL', async () => {
    await open('#/advanced/actions')
    expect(screen.getByTestId('actions-page')).toBeInTheDocument()
    expect(screen.getByTestId('nav-actions')).toHaveAttribute('aria-current', 'page')
  })

  it('renders Not found for the unrouted alert-log URL', async () => {
    await open('#/alert-log')
    expect(screen.getByTestId('not-found')).toBeInTheDocument()
    expect(title()).toHaveTextContent('Not found')
    // ux-05c: no page is called Dashboard; the way back names the Inbox.
    expect(screen.getByRole('link', { name: 'Go to Inbox' })).toHaveAttribute('href', '#/')
    expect(screen.queryByText(/go to dashboard/i)).toBeNull()
  })

  it('keeps the filtered case views and the signed-in login route', async () => {
    await open('#/query-hints')
    expect(screen.getByTestId('cases-page')).toHaveTextContent('query_hint')
    await act(async () => {
      window.location.hash = '#/login'
      window.dispatchEvent(new HashChangeEvent('hashchange'))
    })
    expect(screen.getByTestId('profile-page')).toBeInTheDocument()
    expect(title()).toHaveTextContent('Account')
  })

  it('renders Not found for the signed-out link page once signed in', async () => {
    await open('#/link-sso')
    expect(screen.getByTestId('not-found')).toBeInTheDocument()
  })
})
