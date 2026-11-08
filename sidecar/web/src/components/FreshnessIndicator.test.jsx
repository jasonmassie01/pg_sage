import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FreshnessIndicator } from './FreshnessIndicator'

// dashboard-04: the header says whether the page is live, polling (and how
// old its data is), or offline (and when its data is from).
// No concurrent access tests: a presentational component with one local timer.

const NOW = new Date(2026, 10, 8, 14, 30, 0).getTime()
const secondsAgo = s => NOW - s * 1000
const hhmm = ms => {
  const d = new Date(ms)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
const indicator = () => screen.getByTestId('freshness-indicator')

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
})
afterEach(() => { vi.useRealTimers() })

describe('FreshnessIndicator', () => {
  it('says Live while the stream is live', () => {
    render(<FreshnessIndicator status="live" updatedAt={secondsAgo(40)} />)
    expect(screen.getByRole('status')).toHaveTextContent(/^Live$/)
    expect(indicator()).toHaveAttribute('data-status', 'live')
  })

  it('says how old polled data is while reconnecting', () => {
    render(<FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(40)} />)
    expect(screen.getByRole('status')).toHaveTextContent('Polling, updated 40 s ago')
    expect(indicator()).toHaveAttribute('data-status', 'reconnecting')
  })

  it('says when the data is from while offline', () => {
    const at = new Date(2026, 10, 8, 14, 2, 0).getTime()
    render(<FreshnessIndicator status="offline" updatedAt={at} />)
    expect(screen.getByRole('status')).toHaveTextContent('Offline, data from 14:02')
    expect(indicator()).toHaveAttribute('data-status', 'offline')
  })

  it('accepts an ISO timestamp', () => {
    const iso = new Date(secondsAgo(5)).toISOString()
    render(<FreshnessIndicator status="reconnecting" updatedAt={iso} />)
    expect(indicator()).toHaveTextContent('Polling, updated 5 s ago')
  })

  it('ages the label without a parent re-render', () => {
    render(<FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(40)} />)
    act(() => { vi.advanceTimersByTime(20000) })
    expect(indicator()).toHaveTextContent('Polling, updated 1 min ago')
  })

  it('switches from seconds to minutes at 60 s and to a clock time at one hour', () => {
    const { rerender } = render(
      <FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(59)} />,
    )
    expect(indicator()).toHaveTextContent('Polling, updated 59 s ago')
    rerender(<FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(60)} />)
    expect(indicator()).toHaveTextContent('Polling, updated 1 min ago')
    rerender(<FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(3599)} />)
    expect(indicator()).toHaveTextContent('Polling, updated 59 min ago')
    rerender(<FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(3600)} />)
    expect(indicator()).toHaveTextContent(`Polling, updated at ${hhmm(secondsAgo(3600))}`)
  })

  it('never reports a negative age for a timestamp ahead of the clock', () => {
    render(<FreshnessIndicator status="reconnecting" updatedAt={NOW + 90000} />)
    expect(indicator()).toHaveTextContent('Polling, updated 0 s ago')
  })

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['an unparseable string', 'not a time'],
    ['zero', 0],
  ])('says there is no data yet for %s', (_, updatedAt) => {
    const { rerender } = render(
      <FreshnessIndicator status="reconnecting" updatedAt={updatedAt} />,
    )
    expect(indicator()).toHaveTextContent('Polling, no data yet')
    rerender(<FreshnessIndicator status="offline" updatedAt={updatedAt} />)
    expect(indicator()).toHaveTextContent('Offline, no data yet')
  })

  it('renders nothing without a live-events provider (status null)', () => {
    const { container } = render(<FreshnessIndicator status={null} updatedAt={secondsAgo(1)} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing for an unknown status', () => {
    const { container } = render(<FreshnessIndicator status="bogus" updatedAt={secondsAgo(1)} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('hides the ticking age from screen readers so the status is not re-announced', () => {
    render(<FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(40)} />)
    const age = screen.getByText(', updated 40 s ago')
    expect(age).toHaveAttribute('aria-hidden', 'true')
    expect(indicator()).toHaveAttribute('title', expect.stringContaining(hhmm(secondsAgo(40))))
  })

  it('runs no timer while live and stops ticking on unmount', () => {
    const { rerender, unmount } = render(
      <FreshnessIndicator status="live" updatedAt={secondsAgo(40)} />,
    )
    expect(vi.getTimerCount()).toBe(0)
    rerender(<FreshnessIndicator status="reconnecting" updatedAt={secondsAgo(40)} />)
    expect(vi.getTimerCount()).toBe(1)
    unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('moves Live to Polling to Offline and back as the status changes', () => {
    const updatedAt = secondsAgo(10)
    const { rerender } = render(<FreshnessIndicator status="live" updatedAt={updatedAt} />)
    expect(indicator()).toHaveTextContent(/^Live$/)
    rerender(<FreshnessIndicator status="reconnecting" updatedAt={updatedAt} />)
    expect(indicator()).toHaveTextContent('Polling, updated 10 s ago')
    rerender(<FreshnessIndicator status="offline" updatedAt={updatedAt} />)
    expect(indicator()).toHaveTextContent(`Offline, data from ${hhmm(updatedAt)}`)
    rerender(<FreshnessIndicator status="live" updatedAt={updatedAt} />)
    expect(indicator()).toHaveTextContent(/^Live$/)
  })
})
