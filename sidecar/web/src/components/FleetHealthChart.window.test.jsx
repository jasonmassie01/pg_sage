import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FleetHealthChart } from './FleetHealthChart'
import { TimeRangePicker } from './TimeRangePicker'
import { TimeRangeProvider } from '../context/TimeRangeContext'
import { stubFetch } from '../test/fetchStub'
import { hideTab, resetVisibility, showTab } from '../test/visibility'

// dashboard-07: the #/advanced health chart asks for the picker's window,
// computed when it fetches. The 30 s time tick must not refetch it between
// its 60 s polls. The real hook and provider run against stubFetch.
// No concurrent access tests: one chart, single-threaded fake timers.

const HOUR = 60 * 60 * 1000
const NOW = new Date('2026-11-09T10:00:20Z').getTime()

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
  localStorage.clear()
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  resetVisibility()
  localStorage.clear()
})

const advance = ms => act(async () => { await vi.advanceTimersByTimeAsync(ms) })

function renderChart(database = 'all') {
  const fetch = stubFetch({ '/api/v1/fleet/health': { json: { databases: {} } } })
  render(
    <TimeRangeProvider>
      <TimeRangePicker />
      <FleetHealthChart database={database} />
    </TimeRangeProvider>,
  )
  return fetch
}

// windowOf returns the from/to (ms) a chart request asked for.
function windowOf(fetch, call) {
  const url = new URL(fetch.mock.calls[call][0], 'http://sage.test')
  expect(url.pathname).toBe('/api/v1/fleet/health')
  const from = Date.parse(url.searchParams.get('from'))
  const to = Date.parse(url.searchParams.get('to'))
  expect(Number.isFinite(from) && Number.isFinite(to)).toBe(true)
  return { from, to, params: url.searchParams }
}

// expectWindowAt checks a window ends at the minute after `now` (so the
// newest sample is never cut off) and spans `span` ms.
function expectWindowAt(win, now, span) {
  expect(win.to).toBeGreaterThanOrEqual(now)
  expect(win.to - now).toBeLessThan(60000)
  expect(win.to % 60000).toBe(0)
  expect(win.to - win.from).toBe(span)
}

describe('FleetHealthChart window', () => {
  it('asks for the picker window, 24h by default', async () => {
    const fetch = renderChart()
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(1)
    const win = windowOf(fetch, 0)
    expectWindowAt(win, NOW, 24 * HOUR)
    expect(win.params.has('database')).toBe(false)
    expect(screen.getByTestId('fleet-health-chart-empty')).toBeInTheDocument()
  })

  it('keeps the database filter beside the window', async () => {
    const fetch = renderChart('orders')
    await advance(0)
    expect(windowOf(fetch, 0).params.get('database')).toBe('orders')
  })

  it('does not refetch on the 30 s tick and refetches on its 60 s poll', async () => {
    const fetch = renderChart()
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(1)
    await advance(30000)
    expect(fetch).toHaveBeenCalledTimes(1)
    await advance(29999)
    expect(fetch).toHaveBeenCalledTimes(1)
    await advance(1)
    expect(fetch).toHaveBeenCalledTimes(2)
    // The poll computes the window when it fetches, so it has moved on.
    expectWindowAt(windowOf(fetch, 1), NOW + 60000, 24 * HOUR)
    expect(windowOf(fetch, 1).to).toBeGreaterThan(windowOf(fetch, 0).to)
  })

  it('refetches once with the new window when the picker changes', async () => {
    const fetch = renderChart()
    await advance(0)
    fireEvent.change(screen.getByRole('combobox', { name: 'Time range' }),
      { target: { value: '7d' } })
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(2)
    expectWindowAt(windowOf(fetch, 1), NOW, 7 * 24 * HOUR)
    await advance(30000)
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('uses a stored range', async () => {
    localStorage.setItem('pg_sage_range', '1h')
    const fetch = renderChart()
    await advance(0)
    expectWindowAt(windowOf(fetch, 0), NOW, HOUR)
  })

  it('falls back to 24h for a stored unknown range key', async () => {
    localStorage.setItem('pg_sage_range', 'bogus')
    const fetch = renderChart()
    await advance(0)
    expectWindowAt(windowOf(fetch, 0), NOW, 24 * HOUR)
  })

  it('fetches once with a fresh window when a hidden tab is shown', async () => {
    const fetch = renderChart()
    await advance(0)
    act(() => hideTab())
    await advance(90000)
    expect(fetch).toHaveBeenCalledTimes(1)
    act(() => showTab())
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(2)
    expectWindowAt(windowOf(fetch, 1), NOW + 90000, 24 * HOUR)
    await advance(59999)
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(1)
    expect(fetch).toHaveBeenCalledTimes(3)
  })

  it('ends a window that starts on a minute boundary at that minute', async () => {
    vi.setSystemTime(new Date('2026-11-09T10:00:00Z'))
    const fetch = renderChart()
    await advance(0)
    expect(new Date(windowOf(fetch, 0).to).toISOString()).toBe('2026-11-09T10:00:00.000Z')
  })
})

describe('FleetHealthChart answers', () => {
  function renderWith(reply) {
    stubFetch({ '/api/v1/fleet/health': reply })
    render(<TimeRangeProvider><FleetHealthChart database="all" /></TimeRangeProvider>)
  }

  it('shows the server error and no chart when the request fails', async () => {
    renderWith({ status: 503, json: { error: 'history store offline' } })
    await advance(0)
    expect(screen.getByText(/Health history unavailable: history store offline \(503\)/))
      .toBeInTheDocument()
    expect(screen.queryByTestId('fleet-health-chart')).toBeNull()
  })

  it('shows a skeleton until the first answer, then the chart', async () => {
    renderWith({ json: { databases: { orders: [{ t: '2026-11-09T09:00:00Z', health: 90 }] } } })
    expect(screen.getByTestId('fleet-health-chart-skeleton')).toBeInTheDocument()
    await advance(0)
    expect(screen.getByTestId('fleet-health-chart')).toHaveTextContent('Fleet Health Over Time')
    expect(screen.queryByTestId('fleet-health-chart-skeleton')).toBeNull()
  })
})
