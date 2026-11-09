import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { useAPI } from '../hooks/useAPI'
import { Layout } from './Layout'

// dashboard-12: App owns the one /api/v1/databases poll. Layout renders the
// fleet it is given and never polls the fleet itself, not even while App's
// first load is still running (fleetData null).
// No concurrent access tests: one render tree, single-threaded.

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(() => ({ data: null, refetch: vi.fn(), updatedAt: null })),
}))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

const operator = { email: 'op@example.com', role: 'operator' }
const databases = [{ name: 'orders', emergency_stopped: false, status: {} }]
const fleetData = { summary: { emergency_stopped: false }, databases }

function renderLayout(props = {}) {
  return render(
    <LiveEventsProvider>
      <Layout user={operator} databases={databases} selectedDB="orders"
        onSelectDB={() => {}} fleetData={fleetData}
        onFleetChanged={() => {}} {...props}>
        <p>page</p>
      </Layout>
    </LiveEventsProvider>,
  )
}

const requestedURLs = () => useAPI.mock.calls.map(([url]) => url)

beforeEach(() => {
  useAPI.mockClear()
  window.location.hash = '#/'
})
afterEach(() => {
  vi.unstubAllGlobals()
  window.location.hash = ''
})

describe('Layout fleet data', () => {
  it('makes no /api/v1/databases request when fleetData is given', () => {
    renderLayout()
    expect(screen.getByText('page')).toBeInTheDocument()
    expect(requestedURLs()).toContain('/api/v1/actions/pending/count')
    expect(requestedURLs()).not.toContain('/api/v1/databases')
  })

  it('makes no /api/v1/databases request while App is still loading', () => {
    renderLayout({ fleetData: null })
    expect(screen.getByText('page')).toBeInTheDocument()
    expect(useAPI).toHaveBeenCalled()
    expect(requestedURLs()).not.toContain('/api/v1/databases')
  })

  it('asks App to refetch the fleet after a confirmed stop', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200 }))
    vi.stubGlobal('fetch', fetchMock)
    const onFleetChanged = vi.fn()
    renderLayout({ onFleetChanged })
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    await act(async () => {
      fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
    })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/emergency-stop?database=orders',
      expect.objectContaining({ method: 'POST' }))
    expect(onFleetChanged).toHaveBeenCalledTimes(1)
  })

  it('still asks App to refetch when the stop request fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => {
      throw new TypeError('network down')
    }))
    const onFleetChanged = vi.fn()
    renderLayout({ onFleetChanged })
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    await act(async () => {
      fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
    })
    expect(screen.getByText('network down')).toHaveAttribute('role', 'alert')
    expect(onFleetChanged).toHaveBeenCalledTimes(1)
  })
})
