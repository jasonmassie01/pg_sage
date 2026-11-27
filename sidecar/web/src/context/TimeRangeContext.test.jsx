import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { TimeRangeProvider, useTimeRange } from './TimeRangeContext'
import { hideTab, resetVisibility, showTab } from '../test/visibility'

// dashboard-08: the 30 s time tick moves every from/to URL, so it must
// not run in a hidden tab; showing the tab ticks once if a tick was due.
// No concurrent access tests: one provider, single-threaded timers.

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-09T10:00:00Z'))
})
afterEach(() => {
  vi.useRealTimers()
  resetVisibility()
})

const advance = ms => act(() => { vi.advanceTimersByTime(ms) })
const wrapper = ({ children }) => <TimeRangeProvider>{children}</TimeRangeProvider>

describe('TimeRangeProvider tick', () => {
  it('moves the window every 30 s while visible', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    expect(result.current.toISO).toBe('2026-11-09T10:00:00.000Z')
    advance(29999)
    expect(result.current.toISO).toBe('2026-11-09T10:00:00.000Z')
    advance(1)
    expect(result.current.toISO).toBe('2026-11-09T10:00:30.000Z')
    expect(result.current.fromISO).toBe('2026-11-08T10:00:30.000Z')
  })

  it('does not tick while hidden and ticks once when shown', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    act(() => hideTab())
    advance(120000)
    expect(result.current.toISO).toBe('2026-11-09T10:00:00.000Z')
    act(() => showTab())
    expect(result.current.toISO).toBe('2026-11-09T10:02:00.000Z')
    const shown = result.current
    act(() => showTab()) // a second visible event without a due tick
    expect(result.current).toBe(shown)
  })

  it('does not tick on show when the last tick is under 30 s old', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    advance(30000)
    act(() => hideTab())
    advance(29999)
    act(() => showTab())
    expect(result.current.toISO).toBe('2026-11-09T10:00:30.000Z')
    advance(1) // the regular tick still lands on time
    expect(result.current.toISO).toBe('2026-11-09T10:01:00.000Z')
  })

  it('counts the next tick from the tick on show', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    act(() => hideTab())
    advance(59000)
    act(() => showTab())
    expect(result.current.toISO).toBe('2026-11-09T10:00:59.000Z')
    const shown = result.current
    advance(1000) // the old 30 s schedule would tick again here, 1 s later
    expect(result.current).toBe(shown)
    advance(28999)
    expect(result.current.toISO).toBe('2026-11-09T10:00:59.000Z')
    advance(1)
    expect(result.current.toISO).toBe('2026-11-09T10:01:29.000Z')
  })

  it('ignores a visibilitychange that leaves the tab hidden', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    act(() => hideTab())
    advance(60000)
    act(() => hideTab())
    expect(result.current.toISO).toBe('2026-11-09T10:00:00.000Z')
  })

  it('stops ticking after unmount', () => {
    const removed = vi.spyOn(document, 'removeEventListener')
    const { unmount } = renderHook(() => useTimeRange(), { wrapper })
    unmount()
    expect(removed.mock.calls.some(c => c[0] === 'visibilitychange')).toBe(true)
    expect(() => advance(60000)).not.toThrow()
    removed.mockRestore()
  })
})

// dashboard-07: windowAt(ms) is the window a fetch asks for at that time,
// ending at the next minute boundary so the newest row is never cut off.
// Its identity changes only with the range, never with the 30 s tick.
describe('TimeRangeProvider windowAt', () => {
  afterEach(() => localStorage.clear())

  it('ends the window at the next minute and spans the range', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    const win = result.current.windowAt(Date.parse('2026-11-09T10:00:20Z'))
    expect(win.toISO).toBe('2026-11-09T10:01:00.000Z')
    expect(win.fromISO).toBe('2026-11-08T10:01:00.000Z')
    expect(win.to.getTime() - win.from.getTime()).toBe(24 * 60 * 60 * 1000)
  })

  it('keeps a time on a minute boundary as the window end', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    const win = result.current.windowAt(Date.parse('2026-11-09T10:00:00Z'))
    expect(win.toISO).toBe('2026-11-09T10:00:00.000Z')
  })

  it('keeps its identity across ticks and changes it with the range', () => {
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    const first = result.current.windowAt
    advance(30000)
    expect(result.current.toISO).toBe('2026-11-09T10:00:30.000Z')
    expect(result.current.windowAt).toBe(first)
    act(() => result.current.setRangeKey('1h'))
    expect(result.current.windowAt).not.toBe(first)
    const win = result.current.windowAt(Date.parse('2026-11-09T10:00:00Z'))
    expect(win.fromISO).toBe('2026-11-09T09:00:00.000Z')
  })

  it('reads an unknown stored key as 24h', () => {
    localStorage.setItem('pg_sage_range', 'bogus')
    const { result } = renderHook(() => useTimeRange(), { wrapper })
    expect(result.current.rangeKey).toBe('24h')
    expect(result.current.range.key).toBe('24h')
    expect(result.current.toISO).toBe('2026-11-09T10:00:00.000Z')
    expect(result.current.fromISO).toBe('2026-11-08T10:00:00.000Z')
  })

  it('gives a 24h windowAt without a provider', () => {
    const { result } = renderHook(() => useTimeRange())
    const win = result.current.windowAt(Date.parse('2026-11-09T10:00:20Z'))
    expect(win.toISO).toBe('2026-11-09T10:01:00.000Z')
    expect(win.fromISO).toBe('2026-11-08T10:01:00.000Z')
  })
})
