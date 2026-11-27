import { act, render, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useAPI, withTimeRange } from './useAPI'
import { TimeRangeProvider, useTimeRange } from '../context/TimeRangeContext'
import { stubFetch } from '../test/fetchStub'
import { hideTab, resetVisibility, showTab } from '../test/visibility'

// dashboard-08: a hidden tab stops polling, a failing endpoint is polled
// less and less often, and refetch() stays immediate. The real hook runs
// against stubFetch (real Response objects) under fake timers.

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-09T10:00:00Z'))
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  resetVisibility()
})

const advance = ms => act(async () => { await vi.advanceTimersByTimeAsync(ms) })
const show = () => act(async () => { showTab() })
const hide = () => act(async () => { hideTab() })

function WindowedProbe({ url }) {
  const range = useTimeRange()
  useAPI(withTimeRange(url, range), 30000)
  return null
}

describe('useAPI in a hidden tab', () => {
  it('skips poll and time ticks while hidden and fetches once when shown', async () => {
    const fetch = stubFetch({ '/api/v1/actions': { json: { ok: true } } })
    hideTab()
    render(<TimeRangeProvider><WindowedProbe url="/api/v1/actions" /></TimeRangeProvider>)
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(1)
    await advance(120000)
    expect(fetch).toHaveBeenCalledTimes(1)
    await show()
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(2)
    // The one fetch on show asks for the window that ends now.
    expect(fetch.mock.calls[1][0]).toContain('to=2026-11-09T10%3A02%3A00')
    expect(fetch.mock.calls[1][1].signal.aborted).toBe(false)
  })

  it('resumes the interval after the fetch on show', async () => {
    const fetch = stubFetch({ '/api/v1/health': { json: { ok: true } } })
    renderHook(() => useAPI('/api/v1/health', 30000))
    await advance(0)
    await hide()
    await advance(90000)
    expect(fetch).toHaveBeenCalledTimes(1)
    await show()
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(29999)
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(1)
    expect(fetch).toHaveBeenCalledTimes(3)
  })

  it('does not fetch on show when the next poll was not due yet', async () => {
    const fetch = stubFetch({ '/api/v1/health': { json: { ok: true } } })
    renderHook(() => useAPI('/api/v1/health', 30000))
    await advance(0)
    await hide()
    await advance(10000)
    await show()
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(1)
    await advance(20000)
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('fetches nothing on show for interval 0 or a null url', async () => {
    const fetch = stubFetch({ '/api/v1/cases': { json: { ok: true } } })
    renderHook(() => useAPI('/api/v1/cases', 0))
    renderHook(() => useAPI(null, 30000))
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(1)
    await hide()
    await advance(120000)
    await show()
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it('still fetches at once on refetch() while hidden', async () => {
    const fetch = stubFetch({ '/api/v1/cases': { json: { ok: true } } })
    const { result } = renderHook(() => useAPI('/api/v1/cases', 30000))
    await advance(0)
    await hide()
    await act(async () => { await result.current.refetch() })
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('fetches nothing after unmount when the tab is shown', async () => {
    const fetch = stubFetch({ '/api/v1/health': { json: { ok: true } } })
    const { unmount } = renderHook(() => useAPI('/api/v1/health', 30000))
    await advance(0)
    await hide()
    await advance(60000)
    unmount()
    await show()
    await advance(60000)
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it('wakes every hidden poller once when the tab is shown', async () => {
    const fetch = stubFetch({
      '/api/v1/a': { json: { n: 'a' } },
      '/api/v1/b': { json: { n: 'b' } },
    })
    renderHook(() => useAPI('/api/v1/a', 30000))
    renderHook(() => useAPI('/api/v1/b', 60000))
    await advance(0)
    await hide()
    await advance(120000)
    expect(fetch).toHaveBeenCalledTimes(2)
    await show()
    await advance(0)
    const urls = fetch.mock.calls.slice(2).map(c => c[0]).sort()
    expect(urls).toEqual(['/api/v1/a', '/api/v1/b'])
  })
})

describe('useAPI backoff after failures', () => {
  function flaky() {
    const state = { fail: true }
    const fetch = stubFetch({
      '/api/v1/cases': () => (state.fail
        ? { status: 503, json: { error: 'sidecar down' } }
        : { json: { ok: true } }),
    })
    return { state, fetch }
  }

  it('waits 2x, 4x and caps at 300 s, then one success restores 30 s', async () => {
    const { state, fetch } = flaky()
    const { result } = renderHook(() => useAPI('/api/v1/cases', 30000))
    await advance(0) // failure 1
    expect(result.current.error).toBe('sidecar down (503)')
    await advance(30000) // one failure keeps the interval: failure 2
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(59999)
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(1) // 2x: failure 3
    expect(fetch).toHaveBeenCalledTimes(3)
    await advance(119999)
    expect(fetch).toHaveBeenCalledTimes(3)
    await advance(1) // after 3 failures the wait is 4x: failure 4
    expect(fetch).toHaveBeenCalledTimes(4)
    await advance(240000) // 8x: failure 5
    expect(fetch).toHaveBeenCalledTimes(5)
    await advance(299999) // 16x would be 480 s; the cap is 300 s
    expect(fetch).toHaveBeenCalledTimes(5)
    await advance(1)
    expect(fetch).toHaveBeenCalledTimes(6)
    state.fail = false
    await advance(300000) // still capped; this one succeeds
    expect(fetch).toHaveBeenCalledTimes(7)
    expect(result.current.error).toBeNull()
    await advance(30000)
    expect(fetch).toHaveBeenCalledTimes(8)
  })

  it('keeps refetch() immediate during a backoff and resets it on success', async () => {
    const { state, fetch } = flaky()
    const { result } = renderHook(() => useAPI('/api/v1/cases', 30000))
    await advance(0)
    await advance(30000)
    await advance(60000)
    expect(fetch).toHaveBeenCalledTimes(3)
    state.fail = false
    await act(async () => { await result.current.refetch() })
    expect(fetch).toHaveBeenCalledTimes(4)
    expect(result.current.data).toEqual({ ok: true })
    await advance(29999)
    expect(fetch).toHaveBeenCalledTimes(4)
    await advance(1)
    expect(fetch).toHaveBeenCalledTimes(5)
  })

  it('never polls a long interval faster than the interval itself', async () => {
    const { fetch } = flaky()
    renderHook(() => useAPI('/api/v1/cases', 600000))
    await advance(0)
    await advance(600000)
    await advance(599999)
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(1)
    expect(fetch).toHaveBeenCalledTimes(3)
  })

  it('starts a new resource without the old resource\'s backoff', async () => {
    const fetch = stubFetch({
      '/api/v1/cases?database=a': { status: 503 },
      '/api/v1/cases?database=b': { json: { db: 'b' } },
    })
    const { rerender } = renderHook(({ url }) => useAPI(url, 30000), {
      initialProps: { url: '/api/v1/cases?database=a' },
    })
    await advance(0)
    await advance(30000)
    await advance(60000)
    expect(fetch).toHaveBeenCalledTimes(3)
    rerender({ url: '/api/v1/cases?database=b' })
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(4)
    await advance(30000)
    expect(fetch).toHaveBeenCalledTimes(5)
    expect(fetch.mock.calls[4][0]).toBe('/api/v1/cases?database=b')
  })

  it('keeps backing off when the time-range tick moves a windowed url', async () => {
    const state = { fail: true }
    const fetch = stubFetch({
      '/api/v1/actions': () => (state.fail ? { status: 503 } : { json: { ok: true } }),
    })
    render(<TimeRangeProvider><WindowedProbe url="/api/v1/actions" /></TimeRangeProvider>)
    await advance(30000) // failures 1 and 2: the interval and the first tick
    const afterTwo = fetch.mock.calls.length
    expect(afterTwo).toBeGreaterThanOrEqual(2)
    expect(afterTwo).toBeLessThanOrEqual(3)
    await advance(59999) // ticks at 60 s move the window but wait out the 2x backoff
    expect(fetch).toHaveBeenCalledTimes(afterTwo)
    await advance(1) // 90 s: the backed-off poll asks for the moved window
    expect(fetch).toHaveBeenCalledTimes(afterTwo + 1)
    expect(fetch.mock.calls.at(-1)[0]).toContain('to=2026-11-09T10%3A01%3A00')
    await advance(119999) // three ticks fall inside the 4x wait
    expect(fetch).toHaveBeenCalledTimes(afterTwo + 1)
    state.fail = false
    // 210 s: this poll succeeds and ends the backoff, so the tick due at the
    // same moment refetches the moved window at once again.
    await advance(1)
    const recovered = fetch.mock.calls.length
    expect(recovered).toBeGreaterThanOrEqual(afterTwo + 2)
    expect(recovered).toBeLessThanOrEqual(afterTwo + 3)
    // Healthy again: the window move refetches at once. With zero latency the
    // 30 s poll falls due in the same millisecond, as before dashboard-08.
    await advance(30000)
    expect(fetch.mock.calls.length - recovered).toBeGreaterThanOrEqual(1)
    expect(fetch.mock.calls.length - recovered).toBeLessThanOrEqual(2)
    expect(fetch.mock.calls.at(-1)[0]).toContain('to=2026-11-09T10%3A04%3A00')
  })

  it('does not fetch again when only the interval changes', async () => {
    const fetch = stubFetch({ '/api/v1/onboarding': { json: { ok: true } } })
    const { rerender } = renderHook(({ ms }) => useAPI('/api/v1/onboarding', ms), {
      initialProps: { ms: 10000 },
    })
    await advance(0)
    rerender({ ms: 60000 })
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(1)
    await advance(10000) // the poll already scheduled keeps its time
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(59999)
    expect(fetch).toHaveBeenCalledTimes(2)
    await advance(1)
    expect(fetch).toHaveBeenCalledTimes(3)
  })
})
