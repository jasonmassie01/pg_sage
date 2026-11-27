import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { Layout } from './Layout'

// dashboard-04: the header shows how fresh the data is, driven by the real
// LiveEventsProvider over FakeEventSource; only the fetch layer is stubbed.
// No concurrent access tests: one provider, one header, single-threaded.

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: null, refetch: vi.fn(), updatedAt: null }),
}))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

const NOW = new Date(2026, 10, 8, 14, 30, 0).getTime()
const databases = [{ name: 'orders', emergency_stopped: false, status: {} }]
const fleetData = { summary: { emergency_stopped: false }, databases }
const latest = () => FakeEventSource.instances.at(-1)
const indicator = () => screen.getByTestId('freshness-indicator')

function renderLayout(props = {}) {
  return render(
    <LiveEventsProvider>
      <Layout user={{ email: 'op@example.com', role: 'viewer' }}
        databases={databases} selectedDB="orders" onSelectDB={() => {}}
        fleetData={fleetData} onFleetChanged={() => {}} {...props}>
        <p>page</p>
      </Layout>
    </LiveEventsProvider>,
  )
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
  vi.spyOn(Math, 'random').mockReturnValue(0.5)
  window.location.hash = '#/'
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  window.location.hash = ''
})

describe('Layout freshness indicator', () => {
  it('returns to Live after a sidecar restart without a reload', () => {
    renderLayout({ fleetUpdatedAt: NOW - 40000 })
    expect(indicator()).toHaveTextContent('Polling, updated 40 s ago')
    act(() => latest().open())
    expect(indicator()).toHaveTextContent(/^Live$/)
    // The proxy answers the reconnect with a 502: the browser gives up.
    act(() => latest().fail({ closed: true }))
    expect(indicator()).toHaveTextContent(/^Polling, updated \d+ s ago$/)
    act(() => { vi.advanceTimersByTime(3000) })
    expect(FakeEventSource.instances).toHaveLength(2)
    act(() => latest().open())
    expect(indicator()).toHaveTextContent(/^Live$/)
    expect(indicator().closest('header')).not.toBeNull()
  })

  it('shows Offline with the fleet data time after three failed reconnects', () => {
    const fetchedAt = new Date(2026, 10, 8, 14, 2, 0).getTime()
    renderLayout({ fleetUpdatedAt: fetchedAt })
    act(() => latest().open())
    act(() => latest().fail({ closed: true }))
    act(() => { vi.advanceTimersByTime(3000) })
    act(() => latest().fail({ closed: true }))
    act(() => { vi.advanceTimersByTime(6000) })
    act(() => latest().fail({ closed: true }))
    expect(indicator()).toHaveTextContent('Offline, data from 14:02')
  })

  it('says no data yet when the fleet poll has not answered', () => {
    renderLayout()
    expect(indicator()).toHaveTextContent('Polling, no data yet')
  })
})
