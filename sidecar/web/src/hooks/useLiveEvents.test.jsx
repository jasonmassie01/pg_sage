import { act, render, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { LiveEventsProvider, useLiveEvents, useLiveRefetch } from './useLiveEvents'

// These tests run the real provider against FakeEventSource (installed
// by vitest.setup.js); nothing here mocks useLiveEvents.

function wrapper({ children }) {
  return <LiveEventsProvider>{children}</LiveEventsProvider>
}

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
    const onActions = vi.fn()
    const onFindings = vi.fn()
    render(
      <LiveEventsProvider>
        <Refetcher types={['actions']} refetch={onActions} />
        <Refetcher types={['findings']} refetch={onFindings} />
      </LiveEventsProvider>,
    )
    act(() => onlySource().emit('actions', { type: 'actions', ts: '2026-11-08T10:00:00Z' }))
    expect(onActions).toHaveBeenCalledTimes(1)
    expect(onFindings).not.toHaveBeenCalled()
  })

  it('tracks connected and the last event time', () => {
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    expect(result.current.connected).toBe(false)
    expect(result.current.lastEventAt).toBeNull()
    act(() => onlySource().open())
    expect(result.current.connected).toBe(true)
    act(() => onlySource().emit('heartbeat', { type: 'heartbeat', ts: '2026-11-08T10:00:15Z' }))
    expect(result.current.lastEventAt).toBe('2026-11-08T10:00:15Z')
    act(() => onlySource().fail())
    expect(result.current.connected).toBe(false)
  })

  it('dispatches unnamed messages through onmessage', () => {
    const handler = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => { result.current.subscribe(['health'], handler) })
    act(() => onlySource().emit('message', { type: 'health', database: 'db1' }))
    expect(handler).toHaveBeenCalledWith({ type: 'health', database: 'db1' })
  })

  it('ignores malformed data and keeps dispatching', () => {
    const everything = vi.fn()
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    act(() => { result.current.subscribe(null, everything) })
    const es = onlySource()
    expect(() => act(() => es.emit('actions', 'not json {'))).not.toThrow()
    expect(() => act(() => es.emit('actions', ''))).not.toThrow()
    expect(everything).not.toHaveBeenCalled()
    expect(result.current.lastEventAt).toBeNull()
    act(() => es.emit('actions', { type: 'actions', ts: '2026-11-08T10:01:00Z' }))
    expect(everything).toHaveBeenCalledTimes(1)
    expect(result.current.lastEventAt).toBe('2026-11-08T10:01:00Z')
  })

  it('stamps an event with no ts with the current time', () => {
    const { result } = renderHook(() => useLiveEvents(), { wrapper })
    const before = Date.now()
    act(() => onlySource().emit('findings', { type: 'findings' }))
    const stamped = Date.parse(result.current.lastEventAt)
    expect(stamped).toBeGreaterThanOrEqual(before - 1000)
    expect(stamped).toBeLessThanOrEqual(Date.now() + 1000)
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
  it('returns a disconnected no-op shim and opens no source', () => {
    const { result } = renderHook(() => useLiveEvents())
    expect(result.current.connected).toBe(false)
    expect(result.current.lastEventAt).toBeNull()
    const unsubscribe = result.current.subscribe(['actions'], vi.fn())
    expect(typeof unsubscribe).toBe('function')
    expect(() => unsubscribe()).not.toThrow()
    expect(FakeEventSource.instances).toHaveLength(0)
  })
})

describe('useLiveRefetch', () => {
  it('starts refetching once a missing refetch arrives', () => {
    const refetch = vi.fn()
    const { rerender } = render(
      <LiveEventsProvider><Refetcher types={['actions']} refetch={null} /></LiveEventsProvider>,
    )
    act(() => onlySource().emit('actions', { type: 'actions' }))
    rerender(
      <LiveEventsProvider><Refetcher types={['actions']} refetch={refetch} /></LiveEventsProvider>,
    )
    expect(refetch).not.toHaveBeenCalled()
    act(() => onlySource().emit('actions', { type: 'actions' }))
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('follows a change of the type list', () => {
    const refetch = vi.fn()
    const { rerender } = render(
      <LiveEventsProvider><Refetcher types={['actions']} refetch={refetch} /></LiveEventsProvider>,
    )
    rerender(
      <LiveEventsProvider><Refetcher types={['findings']} refetch={refetch} /></LiveEventsProvider>,
    )
    act(() => onlySource().emit('actions', { type: 'actions' }))
    expect(refetch).not.toHaveBeenCalled()
    act(() => onlySource().emit('findings', { type: 'findings' }))
    expect(refetch).toHaveBeenCalledTimes(1)
  })
})
