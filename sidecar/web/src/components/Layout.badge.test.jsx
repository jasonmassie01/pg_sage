import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { useAPI } from '../hooks/useAPI'
import { Layout, usePendingActionsRefetch } from './Layout'

// ux-01b: one nav badge, on Inbox, counting /api/v1/inbox/count total for
// the selected database (or the fleet). A lower bound is "N+", anything
// above 99 is "99+", and Actions keeps no badge.
// No concurrent access tests: one render tree, single-threaded.

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(() => ({ data: null, refetch: vi.fn(), updatedAt: null })),
}))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

const operator = { email: 'op@example.com', role: 'operator' }
const databases = [{ name: 'orders', emergency_stopped: false, status: {} }]
const fleetData = { summary: { emergency_stopped: false }, databases }
const refetch = vi.fn()

function countData(data) {
  useAPI.mockImplementation(url => ({
    data: url?.startsWith('/api/v1/inbox/count') ? data : null, refetch, updatedAt: null,
  }))
}

// Page stands in for a page that decided something and asks for a refresh.
function Page() {
  const refetchBadge = usePendingActionsRefetch()
  return <button type="button" onClick={refetchBadge}>decided</button>
}

function renderLayout(props = {}) {
  return render(
    <LiveEventsProvider>
      <Layout user={operator} databases={databases} selectedDB="orders"
        onSelectDB={() => {}} fleetData={fleetData} onFleetChanged={() => {}} {...props}>
        <Page />
      </Layout>
    </LiveEventsProvider>,
  )
}

const urls = () => useAPI.mock.calls.map(([url]) => url).filter(Boolean)
const inboxBadge = () => screen.getByTestId('nav-inbox').querySelector('[data-testid="nav-badge"]')

beforeEach(() => {
  useAPI.mockReset()
  refetch.mockReset()
  window.location.hash = '#/'
})
afterEach(() => { window.location.hash = '' })

describe('Layout inbox badge', () => {
  it('counts the inbox total of the selected database on the Inbox link only', () => {
    countData({ total: 3, lower_bound: false, errors: [] })
    renderLayout()
    expect(urls()).toContain('/api/v1/inbox/count?database=orders')
    expect(urls().some(u => u.includes('/actions/pending/count'))).toBe(false)
    expect(inboxBadge()).toHaveTextContent(/^3$/)
    expect(within(screen.getByTestId('nav-actions')).queryByTestId('nav-badge')).toBeNull()
  })

  it('counts the fleet in the all-databases view', () => {
    countData({ total: 1, lower_bound: false, errors: [] })
    renderLayout({ selectedDB: 'all' })
    expect(urls()).toContain('/api/v1/inbox/count')
  })

  it('shows N+ for a lower bound and names what was not read', () => {
    countData({ total: 3, lower_bound: true,
      errors: [{ source: 'autonomy_promotion', database: 'orders', error: 'timed out after 2s' }] })
    renderLayout()
    expect(inboxBadge()).toHaveTextContent(/^3\+$/)
    expect(inboxBadge().getAttribute('title'))
      .toBe('At least 3; not read: autonomy promotions (orders): timed out after 2s')
  })

  it('caps the badge at 99+ above 99', () => {
    countData({ total: 99, lower_bound: false, errors: [] })
    const { unmount } = renderLayout()
    expect(inboxBadge()).toHaveTextContent(/^99$/)
    unmount()
    countData({ total: 100, lower_bound: false, errors: [] })
    renderLayout()
    expect(inboxBadge()).toHaveTextContent(/^99\+$/)
    expect(inboxBadge().getAttribute('title')).toBe('100 items need you')
  })

  it('shows no badge for zero, a missing count or a malformed one', () => {
    for (const data of [{ total: 0, lower_bound: false, errors: [] }, null, {},
      { total: '3' }, { total: -2 }, { total: 2.5 }]) {
      countData(data)
      const { unmount } = renderLayout()
      expect(inboxBadge(), JSON.stringify(data)).toBeNull()
      unmount()
    }
  })

  it('shows 0+ when nothing was counted because a source failed', () => {
    countData({ total: 0, lower_bound: true, errors: [{ source: 'fact', error: 'not connected' }] })
    renderLayout()
    expect(inboxBadge()).toHaveTextContent(/^0\+$/)
  })

  it('does not poll the count for a viewer', () => {
    countData({ total: 5 })
    renderLayout({ user: { email: 'v@example.com', role: 'viewer' } })
    expect(urls().some(u => u.startsWith('/api/v1/inbox/count'))).toBe(false)
    expect(inboxBadge()).toBeNull()
  })

  it('lets a page that decided something refetch the badge, which then drops', () => {
    countData({ total: 2, lower_bound: false, errors: [] })
    const { rerender } = renderLayout()
    expect(inboxBadge()).toHaveTextContent(/^2$/)
    fireEvent.click(screen.getByRole('button', { name: 'decided' }))
    expect(refetch).toHaveBeenCalledTimes(1)
    countData({ total: 1, lower_bound: false, errors: [] })
    rerender(
      <LiveEventsProvider>
        <Layout user={operator} databases={databases} selectedDB="orders"
          onSelectDB={() => {}} fleetData={fleetData} onFleetChanged={() => {}}>
          <Page />
        </Layout>
      </LiveEventsProvider>,
    )
    expect(inboxBadge()).toHaveTextContent(/^1$/)
  })
})
