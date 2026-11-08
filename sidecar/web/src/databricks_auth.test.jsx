import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('./hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('./hooks/useLiveEvents', () => ({
  LiveEventsProvider: ({ children }) => children, useLiveRefetch: vi.fn(),
}))
vi.mock('./components/Toast', () => ({ ToastProvider: ({ children }) => children }))

const user = { id: 7, email: 'viewer@example.test', role: 'viewer', auth_mode: 'databricks_app' }
const json = (body, status = 200) => ({ ok: status < 400, status, json: async () => body })

function responses({ mode = 'databricks_app', identity = user } = {}) {
  globalThis.fetch = vi.fn(async (url, options) => {
    if (url === '/api/v1/auth/mode') return json({ mode })
    if (url === '/api/v1/auth/me') return identity ? json(identity) : json({}, 401)
    if (url === '/api/v1/auth/oauth/config') return json({ enabled: false })
    if (url === '/api/v1/auth/login' && options?.method === 'POST') {
      return json({ id: 9, email: 'local@example.test', role: 'operator' })
    }
    return json({})
  })
}

describe('Databricks Apps sign-in', () => {
  beforeEach(() => {
    localStorage.clear()
    window.location.hash = '#/profile'
    api.useAPI.mockReset()
    api.useAPI.mockReturnValue({ data: null, loading: false, error: null, refetch: vi.fn() })
  })
  afterEach(() => { vi.restoreAllMocks() })

  it('opens with the platform identity and no password, local logout or SSO-link flow', async () => {
    responses()
    render(<App />)
    expect(await screen.findByTestId('databricks-sign-in')).toHaveTextContent('Signed in with Databricks')
    expect(screen.getByTestId('user-email')).toHaveTextContent(user.email)
    expect(screen.queryByTestId('login-password')).toBeNull()
    expect(screen.queryByTestId('sign-out-button')).toBeNull()
    expect(screen.queryByTestId('link-sso-button')).toBeNull()
    expect(globalThis.fetch.mock.calls.map(([url]) => url)).toEqual([
      '/api/v1/auth/mode', '/api/v1/auth/me',
    ])
  })

  it.each(['admin', 'operator', 'viewer'])('does not advertise or call MCP tokens for a platform %s', async role => {
    window.location.hash = '#/mcp-tokens'
    responses({ identity: { id: 1, email: `${role}@example.test`, role } })
    render(<App />)
    expect(await screen.findByTestId('databricks-mcp-unavailable'))
      .toHaveTextContent('Databricks-managed deployment')
    expect(screen.queryByTestId('nav-mcp-tokens')).toBeNull()
    expect(screen.queryByTestId('nav-agents')).toBeNull()
    expect(screen.queryByTestId('mcp-tokens-page')).toBeNull()
    expect(screen.getByRole('link', { name: 'Go to Dashboard' })).toHaveAttribute('href', '#/')
    expect(globalThis.fetch.mock.calls.some(([url]) => String(url).includes('/mcp/'))).toBe(false)
    expect(api.useAPI.mock.calls.some(([url]) => String(url).includes('/mcp/'))).toBe(false)
  })

  it('explains the unsupported Agents route without calling its APIs', async () => {
    window.location.hash = '#/agents'
    responses({ identity: { id: 1, email: 'admin@example.test', role: 'admin' } })
    render(<App />)
    expect(await screen.findByTestId('databricks-agents-unavailable'))
      .toHaveTextContent('Agent governance unavailable')
    expect(screen.queryByTestId('nav-agents')).toBeNull()
    expect(screen.queryByTestId('nav-mcp-tokens')).toBeNull()
    expect(screen.queryByTestId('agents-page')).toBeNull()
    expect(globalThis.fetch.mock.calls.some(([url]) => String(url).startsWith('/api/v1/agents')
      || String(url).startsWith('/api/v1/mcp'))).toBe(false)
  })

  it('keeps a failed platform identity out of local login and reloads through Apps ingress', async () => {
    window.location.hash = '#/link-sso?grant=unused'
    responses({ identity: null })
    const reloadPage = vi.fn()
    render(<App reloadPage={reloadPage} />)
    expect(await screen.findByTestId('platform-auth-error')).toHaveTextContent('Databricks sign-in required')
    expect(screen.queryByTestId('login-email')).toBeNull()
    expect(screen.queryByTestId('login-password')).toBeNull()
    expect(screen.queryByTestId('app-loaded')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Reload' }))
    expect(reloadPage).toHaveBeenCalledOnce()
    expect(globalThis.fetch.mock.calls.map(([url]) => url)).toEqual([
      '/api/v1/auth/mode', '/api/v1/auth/me',
    ])
    expect(globalThis.fetch).not.toHaveBeenCalledWith(
      expect.stringContaining('link-grant'), expect.anything(),
    )
  })

  it('handles an expired platform identity without exposing the local password form', async () => {
    responses()
    const reloadPage = vi.fn()
    render(<App reloadPage={reloadPage} />)
    await screen.findByTestId('app-loaded')
    act(() => { window.dispatchEvent(new CustomEvent('sage:auth-expired')) })
    expect(screen.getByTestId('platform-auth-error')).toHaveTextContent('Databricks sign-in required')
    expect(screen.queryByTestId('login-password')).toBeNull()
    expect(screen.queryByTestId('app-loaded')).toBeNull()
    // Ingress auth redirects can reject API fetches as CORS failures. Recovery
    // must navigate even when another API attempt would fail.
    globalThis.fetch.mockRejectedValue(new TypeError('Failed to fetch'))
    fireEvent.click(screen.getByRole('button', { name: 'Reload' }))
    expect(reloadPage).toHaveBeenCalledOnce()
    expect(globalThis.fetch).toHaveBeenCalledTimes(2)
  })

  it('fails closed when neither the mode nor identity can be verified', async () => {
    responses({ mode: undefined, identity: null })
    // Explicitly simulate mode-discovery failure rather than selecting session.
    globalThis.fetch.mockImplementation(async url => json({}, url.endsWith('/mode') ? 503 : 401))
    render(<App />)
    expect(await screen.findByTestId('platform-auth-error')).toHaveTextContent('Unable to verify sign-in')
    expect(screen.queryByTestId('login-email')).toBeNull()
    expect(screen.queryByTestId('login-password')).toBeNull()
  })

  it('retries mode and identity discovery when the platform mode is still unknown', async () => {
    responses({ identity: null })
    globalThis.fetch.mockRejectedValue(new TypeError('Failed to fetch'))
    const reloadPage = vi.fn()
    render(<App reloadPage={reloadPage} />)
    await screen.findByTestId('platform-auth-error')
    responses()
    fireEvent.click(screen.getByRole('button', { name: 'Reload' }))
    expect(await screen.findByTestId('app-loaded')).toBeInTheDocument()
    expect(screen.queryByTestId('platform-auth-error')).toBeNull()
    expect(reloadPage).not.toHaveBeenCalled()
    expect(globalThis.fetch.mock.calls.map(([url]) => url)).toEqual([
      '/api/v1/auth/mode', '/api/v1/auth/me',
    ])
  })

  it('preserves standalone password sign-in and its logout control', async () => {
    responses({ mode: 'session', identity: null })
    render(<App />)
    fireEvent.change(await screen.findByTestId('login-email'), { target: { value: 'local@example.test' } })
    fireEvent.change(screen.getByTestId('login-password'), { target: { value: 'local-only-password' } })
    fireEvent.click(screen.getByTestId('login-submit'))
    await screen.findByTestId('app-loaded')
    expect(screen.getByTestId('sign-out-button')).toBeInTheDocument()
    await waitFor(() => {
      const call = globalThis.fetch.mock.calls.find(([url]) => url === '/api/v1/auth/login')
      expect(call[1].credentials).toBe('include')
      expect(JSON.parse(call[1].body)).toEqual({ email: 'local@example.test', password: 'local-only-password' })
    })
  })
})
