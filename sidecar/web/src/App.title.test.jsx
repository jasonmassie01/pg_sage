import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// dashboard-10: every tab names its page, so a screen reader and the
// browser's tab list tell routes apart.
// No concurrent access tests: document.title has one writer, the shell.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('./hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
  withTimeRange: path => path,
}))
vi.mock('./hooks/useLiveEvents', () => ({
  LiveEventsProvider: ({ children }) => children,
  useLiveRefetch: vi.fn(),
}))
vi.mock('./components/Toast', () => ({
  ToastProvider: ({ children }) => children,
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))

const fleet = {
  summary: { emergency_stopped: false },
  databases: [{ id: 7, name: 'orders', status: { trust_level: 'advisory' } }],
}

let role = 'admin'

beforeEach(() => {
  role = 'admin'
  localStorage.clear()
  document.title = 'pg_sage Dashboard'
  window.location.hash = '#/cases'
  vi.spyOn(console, 'error').mockImplementation(() => {})
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => ({
    data: url === '/api/v1/databases' ? fleet : null,
    loading: false, error: null, refetch: vi.fn(),
  }))
  globalThis.fetch = vi.fn(async url => (url === '/api/v1/auth/me'
    ? { ok: true, status: 200, json: async () => ({ email: 'op@example.com', role }) }
    : { ok: true, status: 200, json: async () => ({ mode: 'session' }) }))
})

afterEach(() => {
  window.location.hash = ''
  vi.restoreAllMocks()
})

async function navigate(hash) {
  await act(async () => {
    window.location.hash = hash
    window.dispatchEvent(new HashChangeEvent('hashchange'))
  })
}

describe('App: document title follows the route', () => {
  it('titles the first page and each route it moves to', async () => {
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(document.title).toBe('Issues · pg_sage')
    await navigate('#/actions')
    expect(document.title).toBe('Actions · pg_sage')
    await navigate('#/advanced/autonomy')
    expect(document.title).toBe('Earned autonomy · pg_sage')
  })

  it('keeps the route title when a hash carries a query', async () => {
    window.location.hash = '#/actions?database=orders'
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(document.title).toBe('Actions · pg_sage')
  })

  it('titles an unknown route Not found', async () => {
    window.location.hash = '#/no-such-page'
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(document.title).toBe('Not found · pg_sage')
  })

  it('titles an admin route Access denied for a viewer', async () => {
    role = 'viewer'
    window.location.hash = '#/users'
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(document.title).toBe('Access denied · pg_sage')
  })
})
