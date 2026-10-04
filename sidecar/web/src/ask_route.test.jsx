import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// Ask Sage is a top-level page: "Ask Sage" in the nav right after Facts,
// visible to every role, and the #/ask route renders AskPage for the
// selected database.

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
    if (path.endsWith('/ask/budget')) {
      return json({ day: '2026-10-04', database_used: 0, database_limit: 100,
        user_used: 0, user_limit: 20 })
    }
    if (path.endsWith('/ask/conversations')) return json({ conversations: [] })
    return json({})
  })
}

const askCalls = () => globalThis.fetch.mock.calls
  .map(([url]) => String(url)).filter(url => url.includes('/ask'))

describe('Ask Sage route', () => {
  beforeEach(() => {
    window.location.hash = '#/ask'
    localStorage.clear()
    localStorage.setItem('pg_sage_db', 'orders')
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url === '/api/v1/databases' ? fleet
        : url === '/api/v1/actions/pending/count' ? { count: 0 } : null,
      loading: false, error: null, refetch: vi.fn(),
    }))
  })

  afterEach(() => {
    window.location.hash = '#/'
    vi.restoreAllMocks()
  })

  it.each(['viewer', 'operator', 'admin'])(
    'links Ask Sage after Facts and opens the page for a %s', async role => {
      signIn(role)
      render(<App />)
      await screen.findByTestId('app-loaded')
      const link = screen.getByTestId('nav-ask')
      expect(link).toHaveAccessibleName('Ask Sage')
      expect(link).toHaveAttribute('href', '#/ask')
      const facts = screen.getByTestId('nav-facts')
      expect(facts.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING)
        .toBeTruthy()
      expect(screen.getByRole('heading', { level: 1, name: 'Ask Sage' }))
        .toBeInTheDocument()
      expect(await screen.findByTestId('ask-page')).toBeInTheDocument()
      expect(screen.queryByTestId('access-denied')).toBeNull()
      expect(screen.queryByTestId('not-found')).toBeNull()
    })

  it('asks the selected database for its budget and conversations', async () => {
    signIn('operator')
    render(<App />)
    await screen.findByTestId('ask-page')
    await vi.waitFor(() => expect(askCalls()).toEqual(expect.arrayContaining([
      '/api/v1/databases/orders/ask/budget',
      '/api/v1/databases/orders/ask/conversations',
    ])))
  })

  it('asks to pick one database in fleet mode and makes no Ask calls', async () => {
    localStorage.setItem('pg_sage_db', 'all')
    signIn('operator')
    render(<App />)
    expect(await screen.findByTestId('ask-pick-database')).toBeInTheDocument()
    expect(screen.queryByTestId('ask-input')).toBeNull()
    expect(askCalls()).toEqual([])
  })
})
