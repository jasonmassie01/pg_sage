import { act, render, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import {
  LIVE_DEBOUNCE_MS, LiveEventsProvider, useLiveEvents, useLiveRefetch,
} from './useLiveEvents'
import { useLiveStatus } from './liveEventsContext'

// These tests run the real provider against FakeEventSource (installed
// by vitest.setup.js); nothing here mocks useLiveEvents.

function wrapper({ children }) {
  return <LiveEventsProvider>{children}</LiveEventsProvider>
}

afterEach(() => vi.useRealTimers())

// useLiveRefetch refetches LIVE_DEBOUNCE_MS after the last event (dashboard-08).
const settle = () => act(() => { vi.advanceTimersByTime(LIVE_DEBOUNCE_MS) })

function onlySource() {
  expect(FakeEventSource.instances).toHaveLength(1)
  return FakeEventSource.instances[0]
}

function Refetcher({ types, refetch }) {
  useLiveRefetch(types, refetch)
  return null
}

describe('LiveEventsProvider', () => {
  it('opens one credentialed source on /api/v1/events', () => {
    render(
      <LiveEventsProvider>
        <Refetcher types={['actions']} refetch={vi.fn()} />
        <Refetcher types={['findings']} refetch={vi.fn()} />
      </LiveEventsProvider>,
    )
    const es = onlySource()
    expect(es.url).toBe('/api/v1/events')
    expect(es.withCredentials).toBe(true)
    expect(es.readyState).toBe(FakeEventSource.CONNECTING)
  })

  it('delivers an actions event to an actions subscriber only', () => {
    vi.useFakeTimers()
    const onActions = vi.fn()
    const onFindings = vi.fn()
    render(
      <LiveEventsProvider>
        <Refetcher types={['actions']} refetch={onActions} />
        <Refetcher types={['findings']} refetch={onFindings} />
      </LiveEventsProvider>,
    )
    act(() => onlySource().emit('actions', { type: 'actions', ts: '2026-11-08T10:00:00Z' }))
    settle()
    expect(onActions).toHaveBeenCalledTimes(1)
    expect(onFindings).not.toHaveBeenCalled()
  })

  it('reports reconnecting until the stream opens, then live', () => {
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    expect(result.current.status).toBe('reconnecting')
    act(() => onlySource().open())
    expect(result.current.status).toBe('live')
    act(() => onlySource().emit('heartbeat', { type: 'heartbeat', ts: '2026-11-08T10:00:15Z' }))
    expect(result.current.status).toBe('live')
  })

  it('dispatches unnamed messages through onmessage', () => {
    const handler = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => { result.current.subscribe(['health'], handler) })
    act(() => onlySource().emit('message', { type: 'health', database: 'db1' }))
    expect(handler).toHaveBeenCalledWith({ type: 'health', database: 'db1' })
  })

  it('ignores malformed data, keeps dispatching and stays live', () => {
    const everything = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => { result.current.subscribe(null, everything) })
    const es = onlySource()
    act(() => es.open())
    expect(() => act(() => es.emit('actions', 'not json {'))).not.toThrow()
    expect(() => act(() => es.emit('actions', ''))).not.toThrow()
    expect(everything).not.toHaveBeenCalled()
    expect(result.current.status).toBe('live')
    act(() => es.emit('actions', { type: 'actions', ts: '2026-11-08T10:01:00Z' }))
    expect(everything).toHaveBeenCalledTimes(1)
    expect(everything).toHaveBeenCalledWith({ type: 'actions', ts: '2026-11-08T10:01:00Z' })
    expect(result.current.status).toBe('live')
  })

  it('closes the source on unmount', () => {
    const { unmount } = render(<Refetcher types={['actions']} refetch={vi.fn()} />, { wrapper })
    const es = onlySource()
    unmount()
    expect(es.closeCalls).toBe(1)
    expect(es.readyState).toBe(FakeEventSource.CLOSED)
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('keeps dispatching after a listener throws', () => {
    const order = []
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => {
      result.current.subscribe(['actions'], () => { order.push('first'); throw new Error('boom') })
      result.current.subscribe(['actions'], () => order.push('second'))
    })
    expect(() => act(() => onlySource().emit('actions', { type: 'actions' }))).not.toThrow()
    expect(order).toEqual(['first', 'second'])
  })

  it('stops delivering after unsubscribe', () => {
    const handler = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    let unsubscribe
    act(() => { unsubscribe = result.current.subscribe(['actions'], handler) })
    act(() => unsubscribe())
    act(() => onlySource().emit('actions', { type: 'actions' }))
    expect(handler).not.toHaveBeenCalled()
  })

  it('delivers every type to a subscriber with no type list', () => {
    const handler = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => { result.current.subscribe(null, handler) })
    act(() => {
      onlySource().emit('actions', { type: 'actions' })
      onlySource().emit('findings', { type: 'findings' })
    })
    expect(handler.mock.calls.map(c => c[0].type)).toEqual(['actions', 'findings'])
  })

  it('delivers nothing to a subscriber with an empty type list', () => {
    const handler = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => { result.current.subscribe([], handler) })
    act(() => onlySource().emit('actions', { type: 'actions' }))
    expect(handler).not.toHaveBeenCalled()
  })
})

describe('useLiveEvents outside a provider', () => {
  it('returns a no-op shim with no status and opens no source', () => {
    const { result } = renderHook(() => useLiveEvents())
    expect(result.current.status).toBeNull()
    const unsubscribe = result.current.subscribe(['actions'], vi.fn())
    expect(typeof unsubscribe).toBe('function')
    expect(() => unsubscribe()).not.toThrow()
    expect(FakeEventSource.instances).toHaveLength(0)
  })

  it('reports no live status, so no indicator claims a stream', () => {
    const { result } = renderHook(() => useLiveStatus())
    expect(result.current).toBeNull()
  })
})

describe('useLiveStatus inside a provider', () => {
  it('follows the provider status', () => {
    const { result } = renderHook(() => useLiveStatus(), { wrapper })
    expect(result.current).toBe('reconnecting')
    act(() => onlySource().open())
    expect(result.current).toBe('live')
  })
})

describe('useLiveRefetch', () => {
  it('starts refetching once a missing refetch arrives', () => {
    vi.useFakeTimers()
    const refetch = vi.fn()
    const { rerender } = render(
      <LiveEventsProvider><Refetcher types={['actions']} refetch={null} /></LiveEventsProvider>,
    )
    act(() => onlySource().emit('actions', { type: 'actions' }))
    rerender(
      <LiveEventsProvider><Refetcher types={['actions']} refetch={refetch} /></LiveEventsProvider>,
    )
    settle()
    expect(refetch).not.toHaveBeenCalled()
    act(() => onlySource().emit('actions', { type: 'actions' }))
    settle()
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('follows a change of the type list', () => {
    vi.useFakeTimers()
    const refetch = vi.fn()
    const { rerender } = render(
      <LiveEventsProvider><Refetcher types={['actions']} refetch={refetch} /></LiveEventsProvider>,
    )
    rerender(
      <LiveEventsProvider><Refetcher types={['findings']} refetch={refetch} /></LiveEventsProvider>,
    )
    act(() => onlySource().emit('actions', { type: 'actions' }))
    settle()
    expect(refetch).not.toHaveBeenCalled()
    act(() => onlySource().emit('findings', { type: 'findings' }))
    settle()
    expect(refetch).toHaveBeenCalledTimes(1)
  })
})
