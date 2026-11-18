import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { ROUTES, findRoute, routeHref } from './routes'
import { gradePayload } from './test/reviewFixtures'

// review-01b: Review is an Operate page at #/review, for every role, and
// #/review?db=<name> opens that database's review. No concurrent access
// tests: one render of the app shell.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('./hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
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
  databases: [{ id: 7, name: 'orders', status: { trust_level: 'advisory' } },
    { id: 8, name: 'billing', status: { trust_level: 'advisory' } }],
}

describe('Review route', () => {
  beforeEach(() => {
    localStorage.clear()
    globalThis.fetch = vi.fn(async url => {
      const body = String(url).startsWith('/api/v1/auth/me')
        ? { id: 1, email: 'v@x.test', role: 'viewer' } : {}
      return { ok: true, status: 200, json: async () => body,
        text: async () => JSON.stringify(body) }
    })
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({
      data: url === '/api/v1/databases' ? fleet
        : String(url).includes('/review/grade') ? gradePayload()
          : url === '/api/v1/inbox/count' ? { total: 0 } : null,
      loading: false, error: null, refetch: vi.fn(),
    }))
  })

  it('is registered as an Operate page for every role', () => {
    const route = ROUTES.find(r => r.id === 'review')
    expect(route).toMatchObject({ path: '/review', title: 'Review', navGroup: 'operate',
      testId: 'nav-review' })
    expect(route.roles).toBeUndefined()
    expect(routeHref(route)).toBe('#/review')
    expect(findRoute('#/review?db=orders')).toBe(route)
  })

  it('renders the Review page of the database named in ?db=', async () => {
    window.location.hash = '#/review?db=billing'
    render(<App />)
    expect(await screen.findByTestId('grade-card')).toBeInTheDocument()
    expect(screen.getByTestId('nav-review')).toBeInTheDocument()
    expect(api.useAPI).toHaveBeenCalledWith('/api/v1/databases/billing/review/grade',
      expect.any(Number))
  })
})
