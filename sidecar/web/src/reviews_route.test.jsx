import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// Change Review navigation: "Reviews" in the Operate group (every role),
// the #/reviews route (detail via ?id=&database=), and a command palette
// entry.

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

function signIn(role) {
  globalThis.fetch = vi.fn(async url => {
    const path = String(url).split('?')[0]
    if (path === '/api/v1/auth/me') {
      return { ok: true, status: 200,
        json: async () => ({ id: 1, email: `${role}@x.test`, role }) }
    }
    return { ok: true, status: 200, json: async () => ({}) }
  })
}

const reviewsList = { reviews: [], databases: ['orders'], unavailable: [], partial: false }

describe('Reviews route', () => {
  beforeEach(() => {
    window.location.hash = '#/reviews'
    localStorage.clear()
    localStorage.setItem('pg_sage_db', 'all')
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url === '/api/v1/databases' ? fleet
        : url === '/api/v1/actions/pending/count' ? { count: 0 }
          : url === '/api/v1/change-reviews' ? reviewsList : null,
      loading: false, error: null, refetch: vi.fn(),
    }))
  })

  afterEach(() => {
    window.location.hash = '#/'
    vi.restoreAllMocks()
  })

  it.each(['viewer', 'operator', 'admin'])(
    'links Reviews in the Operate group after Cases for a %s', async role => {
      signIn(role)
      render(<App />)
      await screen.findByTestId('app-loaded')
      const link = screen.getByTestId('nav-reviews')
      expect(link).toHaveAccessibleName('Reviews')
      expect(link).toHaveAttribute('href', '#/reviews')
      const cases = screen.getByTestId('nav-cases')
      expect(cases.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING)
        .toBeTruthy()
      expect(screen.getByRole('heading', { level: 1, name: 'Reviews' }))
        .toBeInTheDocument()
      expect(await screen.findByTestId('reviews-page')).toBeInTheDocument()
      expect(screen.queryByTestId('not-found')).toBeNull()
    })

  it('opens the detail view from a link with ?id= and database', async () => {
    window.location.hash = '#/reviews?id=11111111-1111-4111-8111-111111111111' +
      '&database=orders'
    signIn('viewer')
    render(<App />)
    await screen.findByTestId('app-loaded')
    expect(screen.getByTestId('nav-reviews').getAttribute('style'))
      .toContain('var(--accent)')
    await vi.waitFor(() => expect(api.useAPI.mock.calls.map(([u]) => u)).toContain(
      '/api/v1/databases/orders/change-reviews/11111111-1111-4111-8111-111111111111'))
  })

  it('lists Reviews in the command palette', async () => {
    signIn('viewer')
    window.location.hash = '#/'
    render(<App />)
    await screen.findByTestId('app-loaded')
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true })
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Go to Reviews')).toBeInTheDocument()
    fireEvent.click(within(dialog).getByText('Go to Reviews'))
    expect(window.location.hash).toBe('#/reviews')
  })
})
