import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// dashboard-03: a page whose payload has drifted throws during render. The
// app shell, navigation and emergency stop survive, and navigating away
// renders the next page.
// No concurrent access tests: render-time, single-threaded UI state with
// no shared writers.

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

const fleet = {
  summary: { emergency_stopped: false },
  databases: [{ id: 7, name: 'orders', status: { trust_level: 'advisory' } }],
}

beforeEach(() => {
  localStorage.clear()
  window.location.hash = '#/slos'
  vi.spyOn(console, 'error').mockImplementation(() => {})
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => ({
    // A wrong-shaped SLO payload: a string where the page expects a list.
    data: url === '/api/v1/databases' ? fleet
      : url?.startsWith('/api/v1/sre/slos') ? { slos: 'not-a-list' } : null,
    loading: false,
    error: null,
    refetch: vi.fn(),
  }))
  globalThis.fetch = vi.fn(async url => (url === '/api/v1/auth/me'
    ? { ok: true, status: 200,
      json: async () => ({ email: 'op@example.com', role: 'operator' }) }
    : { ok: true, status: 200, json: async () => ({ mode: 'session' }) }))
})

afterEach(() => {
  window.location.hash = ''
  vi.restoreAllMocks()
})

describe('App: a page render error', () => {
  it('keeps the emergency stop and navigation and shows the page fallback', async () => {
    render(<App />)
    await screen.findByTestId('app-loaded')

    expect(screen.getByTestId('page-error')).toHaveTextContent('This page failed to render')
    expect(screen.getByTestId('sidebar-desktop')).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    expect(screen.getByTestId('header-emergency-stop-dialog')).toBeInTheDocument()
  })

  it('renders the next page after navigating away', async () => {
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(screen.getByTestId('page-error')).toBeInTheDocument()

    await act(async () => {
      window.location.hash = '#/alerts'
      window.dispatchEvent(new HashChangeEvent('hashchange'))
    })

    expect(screen.queryByTestId('page-error')).toBeNull()
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Notifications')
    expect(await screen.findByText(/no notifications sent yet/i)).toBeInTheDocument()
  })
})
