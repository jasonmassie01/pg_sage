import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// apiauth-03: when /auth/me reports must_change_password, the app shows only
// the change form (with sign-out); a successful change opens the app.
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

const forced = { id: 4, email: 'v@x.test', role: 'viewer', password_login: true,
  must_change_password: true }

function mockFetch(me) {
  globalThis.fetch = vi.fn(async url => {
    if (url === '/api/v1/auth/mode') {
      return { ok: true, status: 200, json: async () => ({ mode: 'session' }) }
    }
    if (url === '/api/v1/auth/me') return { ok: true, status: 200, json: async () => me }
    if (url === '/api/v1/auth/password') {
      return { ok: true, status: 200, json: async () => ({ status: 'changed' }) }
    }
    return { ok: false, status: 404, json: async () => null }
  })
}

function databasesPolled() {
  return api.useAPI.mock.calls.some(([url]) => url === '/api/v1/databases')
}

beforeEach(() => {
  window.location.hash = '#/slos'
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => ({
    data: url === '/api/v1/databases' ? { summary: {}, databases: [] } : null,
    loading: false, error: null, refetch: vi.fn(),
  }))
})

afterEach(() => {
  window.location.hash = ''
  vi.restoreAllMocks()
})

describe('App forced password change', () => {
  it('routes a must-change session to the change form and nothing else', async () => {
    mockFetch(forced)
    render(<App />)
    expect(await screen.findByTestId('forced-password-change')).toBeInTheDocument()
    expect(screen.getByTestId('change-password-form')).toBeInTheDocument()
    expect(screen.getByTestId('forced-password-logout')).toBeInTheDocument()
    expect(databasesPolled()).toBe(false)
  })

  it('opens the app after the change succeeds', async () => {
    mockFetch(forced)
    render(<App />)
    await screen.findByTestId('forced-password-change')
    fireEvent.change(screen.getByTestId('current-password'), { target: { value: 'temp-pass' } })
    fireEvent.change(screen.getByTestId('new-password'), { target: { value: 'chosen-pass-12' } })
    fireEvent.change(screen.getByTestId('confirm-password'),
      { target: { value: 'chosen-pass-12' } })
    fireEvent.click(screen.getByTestId('change-password-submit'))
    await screen.findByText('v@x.test')
    expect(screen.queryByTestId('forced-password-change')).toBeNull()
    expect(databasesPolled()).toBe(true)
  })

  it('does not force a user without the flag', async () => {
    mockFetch({ ...forced, must_change_password: false })
    render(<App />)
    await screen.findByText('v@x.test')
    expect(screen.queryByTestId('forced-password-change')).toBeNull()
  })
})
