import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// MCP v2: the MCP tokens page is admin only. Admins get "MCP tokens" in the
// nav and the #/mcp-tokens route; operators and viewers get neither the link
// nor the page, and the page never asks the token API on their behalf.

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

const TOKENS = '/api/v1/mcp/tokens'

function json(body) {
  return { ok: true, status: 200, json: async () => body }
}

function signIn(role) {
  globalThis.fetch = vi.fn(async url => {
    const path = String(url).split('?')[0]
    if (path === '/api/v1/auth/me') return json({ id: 1, email: `${role}@x.test`, role })
    if (path === TOKENS) return json({ tokens: [] })
    if (path === '/api/v1/users') return json({ users: [] })
    return json({})
  })
}

const askedForTokens = () =>
  globalThis.fetch.mock.calls.some(([url]) => String(url).startsWith(TOKENS))
  || api.useAPI.mock.calls.some(([url]) => String(url ?? '').startsWith(TOKENS))

describe('MCP tokens route', () => {
  beforeEach(() => {
    window.location.hash = '#/mcp-tokens'
    localStorage.clear()
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url === '/api/v1/databases' ? fleet
        : url?.startsWith(TOKENS) ? { tokens: [] }
          : url === '/api/v1/inbox/count' ? { total: 0 }
            : null,
      loading: false, error: null, refetch: vi.fn(),
    }))
  })

  afterEach(() => {
    window.location.hash = '#/'
    vi.restoreAllMocks()
  })

  // ux-11: MCP tokens are the admin tab of Agents; #/mcp-tokens opens it.
  it('opens the MCP tokens tab of Agents for an admin', async () => {
    signIn('admin')
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(window.location.hash).toBe('#/agents?tab=tokens')
    expect(screen.queryByTestId('nav-mcp-tokens')).toBeNull()
    expect(screen.getByTestId('nav-agents')).toHaveAttribute('aria-current', 'page')
    expect(screen.getByRole('heading', { level: 1, name: 'Agents' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'MCP tokens' }))
      .toHaveAttribute('aria-selected', 'true')
    expect(await screen.findByTestId('mcp-tokens-page')).toBeInTheDocument()
    expect(screen.queryByTestId('access-denied')).toBeNull()
  })

  it('gives an operator the agent list and never the tokens', async () => {
    signIn('operator')
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(screen.queryByTestId('nav-mcp-tokens')).toBeNull()
    expect(screen.queryByRole('tab', { name: 'MCP tokens' })).toBeNull()
    expect(screen.queryByTestId('mcp-tokens-page')).toBeNull()
    expect(askedForTokens()).toBe(false)
  })

  it('denies a viewer the page and asks for no tokens', async () => {
    signIn('viewer')
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(screen.getByTestId('access-denied')).toBeInTheDocument()
    expect(screen.queryByTestId('mcp-tokens-page')).toBeNull()
    expect(askedForTokens()).toBe(false)
  })
})
