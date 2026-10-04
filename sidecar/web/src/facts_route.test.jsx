import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// The Facts page is a top-level page: "Facts" in the nav, #/facts route,
// and it reads the selected database's facts.

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

describe('Facts route', () => {
  beforeEach(() => {
    window.location.hash = '#/facts'
    localStorage.clear()
    localStorage.setItem('pg_sage_db', 'orders')
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url === '/api/v1/databases' ? fleet
        : url?.startsWith('/api/v1/facts') ? { facts: [], total: 0, errors: [] }
          : null,
      loading: false, error: null, refetch: vi.fn(),
    }))
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, json: async () => ({ email: 'viewer@example.com', role: 'viewer' }),
    })
  })

  afterEach(() => {
    window.location.hash = '#/'
    vi.restoreAllMocks()
  })

  it('links Facts in the nav and opens the Facts page', async () => {
    render(<App />)
    await screen.findByTestId('app-loaded')
    const link = screen.getByTestId('nav-facts')
    expect(link).toHaveAccessibleName('Facts')
    expect(link).toHaveAttribute('href', '#/facts')
    expect(screen.getByRole('heading', { level: 1, name: 'Facts' })).toBeInTheDocument()
    expect(screen.getByTestId('facts-page')).toBeInTheDocument()
    expect(api.useAPI).toHaveBeenCalledWith(
      '/api/v1/facts?database=orders&status=proposed', expect.any(Number))
  })
})
