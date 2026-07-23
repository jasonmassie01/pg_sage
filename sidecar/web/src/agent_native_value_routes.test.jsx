import {
  fireEvent, render, screen, waitFor, within,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

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
}))

vi.mock('./pages/Dashboard', () => ({
  Dashboard: () => <div data-testid="legacy-overview">Legacy overview</div>,
}))

const fleet = {
  summary: { emergency_stopped: false },
  databases: [{
    id: 7,
    name: 'orders',
    status: { trust_level: 'advisory' },
  }],
}

const value = {
  dba_hours_saved: { all_time: 12, this_month: 4, this_week: 1 },
  by_feature: {},
  by_database: [],
  incidents_avoided: { count: 0, credited_hours: 0, detail: [] },
  potential_hours_pending: 0,
  trend_daily: [],
}

describe('agent-native Value routing and navigation', () => {
  beforeEach(() => {
    window.location.hash = '#/'
    localStorage.clear()
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url?.startsWith('/api/v1/value') ? value
        : url === '/api/v1/databases' ? fleet
          : url === '/api/v1/actions/pending/count' ? { count: 0 }
            : null,
      loading: false,
      error: null,
      refetch: vi.fn(),
    }))
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        email: 'operator@example.com',
        role: 'operator',
      }),
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('uses Value as the default landing route instead of telemetry overview',
    async () => {
      render(<App />)

      await waitFor(() => {
        expect(screen.getByTestId('app-loaded')).toBeInTheDocument()
      })
      expect(screen.getByRole('heading', { level: 1, name: 'Value' }))
        .toBeInTheDocument()
      expect(screen.getByRole('heading', { name: /value delivered/i }))
        .toBeInTheDocument()
      expect(screen.queryByTestId('legacy-overview')).not.toBeInTheDocument()
    })

  it('makes Value primary and keeps telemetry behind collapsed Advanced nav',
    async () => {
      render(<App />)
      await screen.findByTestId('app-loaded')

      const valueLink = screen.getByTestId('nav-value')
      expect(valueLink).toHaveAccessibleName('Value')
      expect(valueLink).toHaveAttribute('href', '#/')

      const advanced = screen.getByRole('button', { name: /advanced/i })
      expect(advanced).toHaveAttribute('aria-expanded', 'false')
      expect(screen.queryByRole('link', { name: /findings explorer/i }))
        .not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: /action history/i }))
        .not.toBeInTheDocument()

      fireEvent.click(advanced)
      expect(advanced).toHaveAttribute('aria-expanded', 'true')
      expect(screen.getByRole('link', { name: /findings explorer/i }))
        .toBeInTheDocument()
      expect(screen.getByRole('link', { name: /action history/i }))
        .toBeInTheDocument()
      expect(screen.getByRole('link', { name: /snapshot.*metrics/i }))
        .toBeInTheDocument()
    })

  it('offers Value as the primary command-palette destination', async () => {
    render(<App />)
    await screen.findByTestId('app-loaded')

    fireEvent.keyDown(window, { key: 'k', ctrlKey: true })
    const palette = screen.getByRole('dialog', { name: /command palette/i })
    expect(within(palette).getByRole('button', { name: /go to value/i }))
      .toBeInTheDocument()
    expect(within(palette).queryByRole('button', { name: /go to overview/i }))
      .not.toBeInTheDocument()
  })

  it('keeps the selected fleet database when Value is the landing page',
    async () => {
      localStorage.setItem('pg_sage_db', 'orders')
      render(<App />)
      await screen.findByTestId('app-loaded')

      expect(api.useAPI).toHaveBeenCalledWith(
        '/api/v1/value?database=orders',
        expect.any(Number),
      )
    })
})
