import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// apiauth-02: a session-store outage keeps the operator signed in. At boot a
// 503 auth_store_unavailable from /auth/me shows a retry state, not the
// login page; while signed in, sage:auth-degraded shows a banner and keeps
// the user, and sage:auth-restored clears it.
// No concurrent access tests: render-time, single-threaded UI state.

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
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))

const signedIn = { ok: true, status: 200,
  json: async () => ({ email: 'op@example.com', role: 'operator' }) }
const storeDown = { ok: false, status: 503,
  json: async () => ({ error: 'session store unavailable', code: 'auth_store_unavailable' }) }
const noSession = { ok: false, status: 401, json: async () => ({ error: 'no session' }) }

function mockFetch(meResponses) {
  const queue = [...meResponses]
  globalThis.fetch = vi.fn(async url => {
    if (url === '/api/v1/auth/mode') {
      return { ok: true, status: 200, json: async () => ({ mode: 'session' }) }
    }
    if (url === '/api/v1/auth/me') return queue.length > 1 ? queue.shift() : queue[0]
    return { ok: false, status: 404, json: async () => null }
  })
}

const meCalls = () => globalThis.fetch.mock.calls.filter(([u]) => u === '/api/v1/auth/me').length

beforeEach(() => {
  window.location.hash = '#/slos'
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => ({
    data: url === '/api/v1/databases' ? { summary: {}, databases: [] } : null,
    loading: false, error: null, refetch: vi.fn(),
  }))
})

afterEach(() => {
  vi.useRealTimers()
  window.location.hash = ''
  vi.restoreAllMocks()
})

describe('App during a session-store outage', () => {
  it('shows a retry state, not the login page, when /auth/me is 503 at boot', async () => {
    mockFetch([storeDown, signedIn])
    render(<App />)
    expect(await screen.findByTestId('auth-store-retry'))
      .toHaveTextContent(/session store unavailable/i)
    expect(screen.queryByTestId('login-email')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: /retry now/i }))
    await screen.findByTestId('app-loaded')
    expect(screen.queryByTestId('auth-store-retry')).toBeNull()
  })

  it('retries on its own after the Retry-After interval', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    mockFetch([storeDown, signedIn])
    render(<App />)
    await screen.findByTestId('auth-store-retry')
    expect(meCalls()).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    await screen.findByTestId('app-loaded')
    expect(meCalls()).toBe(2)
  })

  it('still shows the login page for a 401 at boot', async () => {
    mockFetch([noSession])
    render(<App />)
    expect(await screen.findByTestId('login-email')).toBeInTheDocument()
    expect(screen.queryByTestId('auth-store-retry')).toBeNull()
  })

  it('keeps the user and shows a banner on sage:auth-degraded', async () => {
    mockFetch([signedIn])
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(screen.queryByTestId('auth-store-banner')).toBeNull()
    act(() => { window.dispatchEvent(new CustomEvent('sage:auth-degraded')) })
    expect(screen.getByTestId('auth-store-banner'))
      .toHaveTextContent(/session store unavailable, retrying/i)
    expect(screen.getByTestId('app-loaded')).toBeInTheDocument()
    expect(screen.queryByTestId('login-email')).toBeNull()
    act(() => { window.dispatchEvent(new CustomEvent('sage:auth-restored')) })
    expect(screen.queryByTestId('auth-store-banner')).toBeNull()
  })

  it('a later 401 still signs the user out and clears the banner', async () => {
    mockFetch([signedIn])
    render(<App />)
    await screen.findByTestId('app-loaded')
    act(() => { window.dispatchEvent(new CustomEvent('sage:auth-degraded')) })
    act(() => { window.dispatchEvent(new CustomEvent('sage:auth-expired')) })
    expect(await screen.findByTestId('login-email')).toBeInTheDocument()
    expect(screen.queryByTestId('auth-store-banner')).toBeNull()
  })
})
