import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// The Agents page is for operators and admins: they get "Agents" in the
// nav and the #/agents route; viewers get neither, and the page never asks
// the agents API on their behalf.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('./hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))
vi.mock('./hooks/useLiveEvents', () => ({
  LiveEventsProvider: ({ children }) => children,
  useLiveRefetch: vi.fn(),
}))
vi.mock('./components/Toast', () => ({
  ToastProvider: ({ children }) => children,
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const fleet = {
  summary: { emergency_stopped: false },
  databases: [{ id: 7, name: 'orders', status: { trust_level: 'advisory' } }],
}

function json(body) {
  return { ok: true, status: 200, json: async () => body }
}

function signIn(role) {
  globalThis.fetch = vi.fn(async url => {
    const path = String(url).split('?')[0]
    if (path === '/api/v1/auth/me') return json({ id: 1, email: `${role}@x.test`, role })
    if (path === '/api/v1/agents') return json({ items: [], next_cursor: '' })
    if (path === '/api/v1/users') return json({ users: [] })
    return json({})
  })
}

const askedForAgents = () =>
  globalThis.fetch.mock.calls.some(([url]) => String(url).startsWith('/api/v1/agents'))

describe('Agents route', () => {
  beforeEach(() => {
    window.location.hash = '#/agents'
    localStorage.clear()
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url === '/api/v1/databases' ? fleet
        : url === '/api/v1/actions/pending/count' ? { count: 0 } : null,
      loading: false, error: null, refetch: vi.fn(),
    }))
  })

  afterEach(() => {
    window.location.hash = ''
    vi.restoreAllMocks()
  })

  for (const role of ['admin', 'operator']) {
    it(`opens for ${role}s with a nav link`, async () => {
      signIn(role)
      render(<App />)
      expect(await screen.findByTestId('agents-page')).toBeInTheDocument()
      expect(screen.getAllByTestId('nav-agents').length).toBeGreaterThan(0)
    })
  }

  it('is denied to viewers, without a nav link or an API call', async () => {
    signIn('viewer')
    render(<App />)
    expect(await screen.findByTestId('access-denied')).toBeInTheDocument()
    expect(screen.queryByTestId('nav-agents')).toBeNull()
    expect(askedForAgents()).toBe(false)
  })
})
