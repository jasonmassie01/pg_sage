import { render, screen } from '@testing-library/react'
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

describe('signed-in SSO error landing', () => {
  beforeEach(() => {
    localStorage.clear()
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url?.startsWith('/api/v1/value') ? value
        : url === '/api/v1/databases' ? fleet : null,
      loading: false,
      error: null,
      refetch: vi.fn(),
    }))
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ email: 'operator@example.com', role: 'operator' }),
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  // A signed-in user who starts a plain SSO sign-in lands on #/login with an
  // sso_error; the account page must explain it instead of "Not found".
  it('shows the SSO error on the account page, not Not found', async () => {
    window.location.hash = '#/login?sso_error=link_conflict'
    render(<App />)
    await screen.findByTestId('app-loaded')

    expect(screen.queryByText(/not found/i)).not.toBeInTheDocument()
    expect(await screen.findByText(/cannot be linked to this account/i))
      .toBeInTheDocument()
  })
})
