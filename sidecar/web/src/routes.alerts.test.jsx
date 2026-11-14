import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { findRoute, canSee, ROUTES } from './routes'

// alerting-06c: the Alert log is folded into Notifications. /alerts
// redirects to the Notifications page's Log tab, keeping ?database=, and
// the Log tab opens to the roles /alerts had (admins and operators); the
// channel and rule tabs stay admin-only.
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
vi.mock('./pages/notifications/LogTab', () => ({
  LogTab: ({ database }) => <div data-testid="log-tab">{String(database)}</div>,
}))
vi.mock('./pages/notifications/ChannelsTab', () => ({
  ChannelsTab: () => <div data-testid="channels-tab" />,
}))
vi.mock('./pages/notifications/RulesTab', () => ({
  RulesTab: () => <div data-testid="rules-tab" />,
}))

const fleet = {
  summary: { emergency_stopped: false },
  databases: [{ id: 1, name: 'x', status: {} }, { id: 2, name: 'y', status: {} }],
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

describe('/alerts redirect', () => {
  it('lands /alerts?database=x on the Notifications Log tab with database=x', async () => {
    await open('#/alerts?database=x', 'operator')
    await waitFor(() => expect(window.location.hash).toBe('#/notifications?tab=log&database=x'))
    expect(await screen.findByTestId('log-tab')).toHaveTextContent('x')
    expect(title()).toHaveTextContent('Notifications')
  })

  it('redirects a bare /alerts to the Log tab without inventing a database', async () => {
    await open('#/alerts')
    await waitFor(() => expect(window.location.hash).toBe('#/notifications?tab=log'))
    expect(await screen.findByTestId('log-tab')).toBeInTheDocument()
    expect(screen.queryByTestId('channels-tab')).toBeNull()
  })

  it('replaces the history entry, so Back does not bounce through /alerts', async () => {
    const before = window.history.length
    await open('#/alerts?database=y')
    await waitFor(() => expect(window.location.hash).toBe('#/notifications?tab=log&database=y'))
    expect(window.history.length).toBe(before + 1)
  })

  it('still denies a viewer, who never had the alert log', async () => {
    await open('#/alerts?database=x', 'viewer')
    expect(screen.getByTestId('access-denied')).toBeInTheDocument()
    expect(window.location.hash).toBe('#/alerts?database=x')
    expect(screen.queryByTestId('log-tab')).toBeNull()
  })
})

describe('Notifications page audience', () => {
  it('shows an operator only the Log tab', async () => {
    await open('#/notifications', 'operator')
    expect(await screen.findByTestId('log-tab')).toBeInTheDocument()
    expect(screen.queryByTestId('notifications-tab-channels')).toBeNull()
    expect(screen.queryByTestId('notifications-tab-rules')).toBeNull()
    expect(screen.queryByTestId('channels-tab')).toBeNull()
  })

  it('opens an admin on Channels with every tab, and on Log for tab=log', async () => {
    await open('#/notifications')
    expect(screen.getByTestId('channels-tab')).toBeInTheDocument()
    for (const tab of ['channels', 'rules', 'log']) {
      expect(screen.getByTestId(`notifications-tab-${tab}`)).toBeInTheDocument()
    }
  })

  it('falls back to the first tab the role has for an unknown tab', async () => {
    await open('#/notifications?tab=rules', 'operator')
    expect(await screen.findByTestId('log-tab')).toBeInTheDocument()
    expect(screen.queryByTestId('rules-tab')).toBeNull()
  })

  it('opens the route to admins and operators, never viewers', () => {
    const notifications = findRoute('#/notifications?tab=log')
    expect(notifications.id).toBe('notifications')
    expect(canSee(notifications, { role: 'operator' }, false)).toBe(true)
    expect(canSee(notifications, { role: 'admin' }, false)).toBe(true)
    expect(canSee(notifications, { role: 'viewer' }, false)).toBe(false)
    const alerts = ROUTES.find(r => r.id === 'alerts')
    expect(alerts.roles).toEqual(notifications.roles)
  })
})
