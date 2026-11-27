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
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

// ux-01b: the Inbox replaced Value as the landing page; Value stays at
// #/value and the Inbox carries one Value line linking to it.

vi.mock('./pages/fleet/FleetOverview', () => ({
  FleetOverview: () => <div data-testid="legacy-overview">Legacy overview</div>,
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
          : url === '/api/v1/inbox/count' ? { total: 0 }
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

  it('lands on the Inbox with the Value line instead of telemetry overview',
    async () => {
      render(<App />)

      await waitFor(() => {
        expect(screen.getByTestId('app-loaded')).toBeInTheDocument()
      })
      expect(screen.getByRole('heading', { level: 1, name: 'Inbox' }))
        .toBeInTheDocument()
      expect(screen.getByTestId('inbox-value'))
        .toHaveTextContent('Verified DBA hours saved: 1 h this week, 12 h all time.')
      expect(screen.queryByTestId('legacy-overview')).not.toBeInTheDocument()
    })

  // ux-11: Value is the ledger's summary; #/value opens that section.
  it('sends #/value to the ledger with its Value section open', async () => {
    window.location.hash = '#/value'
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(window.location.hash).toBe('#/ledger?section=value')
    expect(screen.getAllByRole('heading', { level: 1, name: 'Outcome ledger' }).length)
      .toBeGreaterThan(0)
    expect(screen.getByRole('heading', { name: /value delivered/i })).toBeInTheDocument()
  })

  it('makes the Inbox primary and keeps evidence behind the collapsed Evidence nav',
    async () => {
      render(<App />)
      await screen.findByTestId('app-loaded')

      const inboxLink = screen.getByTestId('nav-inbox')
      expect(inboxLink).toHaveAccessibleName('Inbox')
      expect(inboxLink).toHaveAttribute('href', '#/')
      expect(screen.queryByTestId('nav-value')).toBeNull()

      const advanced = screen.getByRole('button', { name: 'Evidence' })
      expect(advanced).toHaveAttribute('aria-expanded', 'false')
      expect(screen.queryByRole('link', { name: /findings explorer/i }))
        .not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: /action history/i }))
        .not.toBeInTheDocument()

      fireEvent.click(advanced)
      expect(advanced).toHaveAttribute('aria-expanded', 'true')
      // dashboard-06: Findings explorer and Action history were Cases and
      // Actions again; their URLs stay as aliases but the nav lists them once.
      expect(screen.queryByRole('link', { name: /findings explorer/i }))
        .not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: /action history/i }))
        .not.toBeInTheDocument()
      expect(screen.getByRole('link', { name: /runbooks/i }))
        .toBeInTheDocument()
      // ux-11: Snapshot & metrics is gone (its parts moved to Fleet and Database).
      expect(screen.queryByRole('link', { name: /snapshot.*metrics/i }))
        .not.toBeInTheDocument()
    })

  it('offers the Inbox as the primary command-palette destination', async () => {
    render(<App />)
    await screen.findByTestId('app-loaded')

    // The palette attaches its key listener in an effect that can land after
    // the shell renders on a slow runner; press again only while it is closed
    // (a second press on an open palette would toggle it shut).
    const findPalette = () => screen.queryByRole('dialog', { name: /command palette/i })
    await waitFor(() => {
      if (!findPalette()) fireEvent.keyDown(window, { key: 'k', ctrlKey: true })
      expect(findPalette()).toBeInTheDocument()
    })
    const palette = findPalette()
    expect(within(palette).getByRole('button', { name: /go to inbox/i }))
      .toBeInTheDocument()
    expect(within(palette).queryByRole('button', { name: /go to overview/i }))
      .not.toBeInTheDocument()
  })

  it('keeps the selected fleet database for the Value line of the Inbox',
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
