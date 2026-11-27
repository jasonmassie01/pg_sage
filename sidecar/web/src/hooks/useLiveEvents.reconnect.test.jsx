import { memo, useEffect } from 'react'
import { act, render, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { LiveEventsProvider, useLiveEvents } from './useLiveEvents'

// dashboard-04: a reconnect answered with a non-200 (a proxy 502 during a
// sidecar restart, or the handler's own 500/503) closes an EventSource for
// good. The provider must open a new one after a jittered backoff instead
// of leaving live refresh dead until a page reload.

function wrapper({ children }) {
  return <LiveEventsProvider>{children}</LiveEventsProvider>
}

const latest = () => FakeEventSource.instances.at(-1)
const advance = ms => act(() => { vi.advanceTimersByTime(ms) })
const failClosed = () => act(() => latest().fail({ closed: true }))
const failRetrying = () => act(() => latest().fail())

// waitsExactly proves the next source appears at `ms` and not a tick earlier.
function waitsExactly(ms) {
  const before = FakeEventSource.instances.length
  advance(ms - 1)
  expect(FakeEventSource.instances).toHaveLength(before)
  advance(1)
  expect(FakeEventSource.instances).toHaveLength(before + 1)
}

beforeEach(() => { vi.useFakeTimers() })
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('LiveEventsProvider reconnect', () => {
  it('opens a second source after the browser gives up on the first', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5)
    const handler = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => { result.current.subscribe(['actions'], handler) })
    const first = latest()
    act(() => first.open())
    failClosed()
    expect(first.closeCalls).toBeGreaterThanOrEqual(1)
    expect(result.current.status).toBe('reconnecting')
    expect(FakeEventSource.instances).toHaveLength(1)
    advance(3000)
    expect(FakeEventSource.instances).toHaveLength(2)
    const second = latest()
    expect(second).not.toBe(first)
    expect(second.url).toBe('/api/v1/events')
    expect(second.withCredentials).toBe(true)
    act(() => second.open())
    expect(result.current.status).toBe('live')
    act(() => second.emit('actions', { type: 'actions', ts: '2026-11-08T10:00:00Z' }))
    expect(handler).toHaveBeenCalledTimes(1)
    expect(handler).toHaveBeenCalledWith({ type: 'actions', ts: '2026-11-08T10:00:00Z' })
  })

  it('resets the next delay to 3 s once a source opens', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5)
    renderHook(() => useLiveEvents(), { wrapper })
    failClosed()
    waitsExactly(3000)
    failClosed()
    waitsExactly(6000)
    act(() => latest().open())
    failClosed()
    waitsExactly(3000)
  })

  it('reports offline after three failures and live again on open', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5)
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => latest().open())
    failClosed()
    expect(result.current.status).toBe('reconnecting')
    advance(3000)
    failClosed()
    expect(result.current.status).toBe('reconnecting')
    advance(6000)
    failClosed()
    expect(result.current.status).toBe('offline')
    // Offline still retries, so a recovered sidecar is picked up.
    waitsExactly(12000)
    expect(result.current.status).toBe('offline')
    act(() => latest().open())
    expect(result.current.status).toBe('live')
  })

  it('keeps every delay at or above 80% of its base', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0)
    renderHook(() => useLiveEvents(), { wrapper })
    failClosed()
    waitsExactly(2400)
    failClosed()
    waitsExactly(4800)
  })

  it('keeps every delay at or below 120% of its base, capped at 72 s', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.9999999999)
    renderHook(() => useLiveEvents(), { wrapper })
    for (const ms of [3600, 7200, 14400, 28800, 57600, 72000, 72000, 72000]) {
      failClosed()
      waitsExactly(ms)
    }
  })

  it('creates no source and closes the current one on unmount mid-backoff', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5)
    const { unmount } = renderHook(() => useLiveEvents(), { wrapper })
    failClosed()
    advance(3000)
    const second = latest()
    act(() => second.open())
    failClosed()
    expect(vi.getTimerCount()).toBe(1)
    unmount()
    expect(vi.getTimerCount()).toBe(0)
    advance(10 * 60 * 1000)
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(second.closeCalls).toBeGreaterThanOrEqual(1)
    expect(second.readyState).toBe(FakeEventSource.CLOSED)
  })

  it('closes the open replacement source on unmount', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5)
    const { unmount } = renderHook(() => useLiveEvents(), { wrapper })
    failClosed()
    advance(3000)
    const second = latest()
    act(() => second.open())
    unmount()
    expect(second.closeCalls).toBe(1)
    expect(second.readyState).toBe(FakeEventSource.CLOSED)
    expect(FakeEventSource.instances).toHaveLength(2)
  })

  // A sidecar restart drops the stream (the browser retries, CONNECTING)
  // and the proxy then answers that retry with a 502 (CLOSED). The first
  // reconnect of our own still waits only the base 3 s.
  it('waits 3 s for the first reconnect after a browser retry was refused', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5)
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => latest().open())
    failRetrying()
    failClosed()
    expect(result.current.status).toBe('reconnecting')
    waitsExactly(3000)
    failClosed()
    expect(result.current.status).toBe('offline')
    waitsExactly(6000)
  })

  it('schedules one reconnect when a closed source reports twice', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5)
    renderHook(() => useLiveEvents(), { wrapper })
    const first = latest()
    failClosed()
    act(() => first.fail({ closed: true }))
    advance(60 * 1000)
    expect(FakeEventSource.instances).toHaveLength(2)
  })
})

describe('LiveEventsProvider while the browser retries', () => {
  it('opens no extra source for an error while CONNECTING', () => {
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    const es = latest()
    act(() => es.open())
    failRetrying()
    advance(10 * 60 * 1000)
    expect(FakeEventSource.instances).toHaveLength(1)
    expect(es.closeCalls).toBe(0)
    // One error is the routine 45 s stream rotation; the header stays Live.
    expect(result.current.status).toBe('live')
    act(() => es.open())
    expect(result.current.status).toBe('live')
  })

  it('reports reconnecting, then offline, as browser retries keep failing', () => {
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    const es = latest()
    act(() => es.open())
    failRetrying()
    failRetrying()
    expect(result.current.status).toBe('reconnecting')
    failRetrying()
    expect(result.current.status).toBe('offline')
    expect(FakeEventSource.instances).toHaveLength(1)
    act(() => es.open())
    expect(result.current.status).toBe('live')
  })

  it('reports reconnecting when the first connect fails while CONNECTING', () => {
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    failRetrying()
    expect(result.current.status).toBe('reconnecting')
  })
})

describe('LiveEventsProvider heartbeats', () => {
  it('does not re-render a memoized consumer for ten heartbeats', () => {
    const rendered = vi.fn()
    const heard = vi.fn()
    const Consumer = memo(function Consumer() {
      const { subscribe, status } = useLiveEvents()
      useEffect(() => subscribe(['heartbeat'], heard), [subscribe])
      rendered()
      return <span data-status={status} />
    })
    render(<Consumer />, { wrapper })
    act(() => latest().open())
    const afterOpen = rendered.mock.calls.length
    expect(afterOpen).toBeGreaterThanOrEqual(1)
    for (let i = 0; i < 10; i += 1) {
      const ts = `2026-11-08T10:00:${10 + i}Z`
      act(() => latest().emit('heartbeat', { type: 'heartbeat', ts }))
    }
    expect(rendered).toHaveBeenCalledTimes(afterOpen)
    expect(heard).toHaveBeenCalledTimes(10)
  })

  it('does not re-render a memoized consumer when the provider parent re-renders', () => {
    const rendered = vi.fn()
    const Consumer = memo(function Consumer() {
      useLiveEvents()
      rendered()
      return null
    })
    const child = <Consumer />
    const Parent = ({ tick }) => (
      <div data-tick={tick}><LiveEventsProvider>{child}</LiveEventsProvider></div>
    )
    const { rerender } = render(<Parent tick={1} />)
    act(() => latest().open())
    const afterOpen = rendered.mock.calls.length
    rerender(<Parent tick={2} />)
    rerender(<Parent tick={3} />)
    expect(rendered).toHaveBeenCalledTimes(afterOpen)
    expect(FakeEventSource.instances).toHaveLength(1)
  })
})
