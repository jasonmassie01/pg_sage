import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useAPI, resourceKey, withTimeRange } from './useAPI'
import { stubFetch } from '../test/fetchStub'

afterEach(() => vi.unstubAllGlobals())

// G9-B05: a new from/to window for the same resource must keep the
// current data visible (stale-while-revalidate) instead of flashing a
// skeleton and collapsing expanded rows.
describe('useAPI', () => {
  it('keeps data when only the time window changes', async () => {
    let resolveSecond
    const fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ n: 1 }) })
      .mockImplementationOnce(() => new Promise(r => { resolveSecond = r }))
    vi.stubGlobal('fetch', fetch)
    const { result, rerender } = renderHook(({ url }) => useAPI(url, 0), {
      initialProps: { url: '/api/v1/actions?from=a&to=b' },
    })
    await waitFor(() => expect(result.current.data).toEqual({ n: 1 }))
    rerender({ url: '/api/v1/actions?from=c&to=d' })
    expect(result.current.data).toEqual({ n: 1 })
    expect(result.current.loading).toBe(false)
    await act(async () => {
      resolveSecond({ ok: true, status: 200, json: async () => ({ n: 2 }) })
    })
    await waitFor(() => expect(result.current.data).toEqual({ n: 2 }))
  })

  it('clears data when the resource itself changes', async () => {
    const fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ db: 1 }) })
      .mockImplementationOnce(() => new Promise(() => {}))
    vi.stubGlobal('fetch', fetch)
    const { result, rerender } = renderHook(({ url }) => useAPI(url, 0), {
      initialProps: { url: '/api/v1/actions?database=a' },
    })
    await waitFor(() => expect(result.current.data).toEqual({ db: 1 }))
    rerender({ url: '/api/v1/actions?database=b' })
    expect(result.current.data).toBeNull()
  })

  it('derives the resource key without the time window', () => {
    expect(resourceKey('/x?database=a&from=1&to=2')).toBe('/x?database=a')
    expect(resourceKey('/x?from=1&to=2')).toBe('/x')
    expect(resourceKey(null)).toBe(null)
  })
})

// The real hook against stubFetch (real Response objects, abort-aware).
describe('useAPI with stubFetch', () => {
  function deferred() {
    let resolve
    const promise = new Promise(r => { resolve = r })
    return { promise, resolve }
  }

  it('loads JSON and reports no error', async () => {
    stubFetch({ '/api/v1/findings': { json: { findings: [1, 2] } } })
    const { result } = renderHook(() => useAPI('/api/v1/findings', 0))
    expect(result.current.loading).toBe(true)
    await waitFor(() => expect(result.current.data).toEqual({ findings: [1, 2] }))
    expect(result.current.error).toBeNull()
    expect(result.current.loading).toBe(false)
  })

  it('settles on the second of two refetches when it resolves first', async () => {
    const first = deferred()
    const second = deferred()
    const fetch = stubFetch({
      '/api/v1/actions': [
        { json: { n: 0 } },
        () => first.promise,
        () => second.promise,
      ],
    })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.data).toEqual({ n: 0 }))
    act(() => {
      result.current.refetch()
      result.current.refetch()
    })
    expect(fetch).toHaveBeenCalledTimes(3)
    expect(fetch.mock.calls[1][1].signal.aborted).toBe(true)
    expect(fetch.mock.calls[2][1].signal.aborted).toBe(false)
    // The aborted first request has settled; it must leave no error.
    await act(async () => {})
    expect(result.current.error).toBeNull()
    expect(result.current.data).toEqual({ n: 0 })
    await act(async () => { second.resolve({ json: { n: 2 } }) })
    await waitFor(() => expect(result.current.data).toEqual({ n: 2 }))
    await act(async () => { first.resolve({ json: { n: 1 } }) })
    expect(result.current.data).toEqual({ n: 2 })
    expect(result.current.error).toBeNull()
    expect(result.current.loading).toBe(false)
  })

  it('reports a server error with its status and keeps the old data', async () => {
    stubFetch({
      '/api/v1/actions': [
        { json: { n: 1 } },
        { status: 503, statusText: 'Service Unavailable', text: 'down' },
      ],
    })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.data).toEqual({ n: 1 }))
    await act(async () => { await result.current.refetch() })
    expect(result.current.error).toBe('503 Service Unavailable')
    expect(result.current.data).toEqual({ n: 1 })
  })

  it('reports an expired session on 401', async () => {
    stubFetch({ '/api/v1/actions': { status: 401, json: { error: 'unauthorized' } } })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.error).toBe('session expired'))
    expect(result.current.data).toBeNull()
    expect(result.current.loading).toBe(false)
  })

  it('reports a body that is not JSON', async () => {
    stubFetch({ '/api/v1/actions': { text: '<html>proxy error</html>' } })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.error).toMatch(/JSON/))
    expect(result.current.data).toBeNull()
  })

  it('fetches nothing for a null url', async () => {
    const fetch = stubFetch({})
    const { result } = renderHook(() => useAPI(null, 0))
    await act(async () => { await result.current.refetch() })
    expect(fetch).not.toHaveBeenCalled()
    expect(result.current).toMatchObject({ data: null, error: null, loading: false })
  })

  it('aborts the request in flight on unmount', async () => {
    const pending = deferred()
    const fetch = stubFetch({ '/api/v1/actions': () => pending.promise })
    const { unmount } = renderHook(() => useAPI('/api/v1/actions', 0))
    expect(fetch).toHaveBeenCalledTimes(1)
    unmount()
    expect(fetch.mock.calls[0][1].signal.aborted).toBe(true)
  })

  it('polls at the interval and stops after unmount', async () => {
    vi.useFakeTimers()
    try {
      const fetch = stubFetch({ '/api/v1/health': { json: { ok: true } } })
      const { unmount } = renderHook(() => useAPI('/api/v1/health', 1000))
      expect(fetch).toHaveBeenCalledTimes(1)
      await act(async () => { await vi.advanceTimersByTimeAsync(999) })
      expect(fetch).toHaveBeenCalledTimes(1)
      await act(async () => { await vi.advanceTimersByTimeAsync(1) })
      expect(fetch).toHaveBeenCalledTimes(2)
      unmount()
      await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
      expect(fetch).toHaveBeenCalledTimes(2)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('withTimeRange', () => {
  const range = { fromISO: '2026-11-08T10:00:00Z', toISO: '2026-11-08T11:00:00+01:00' }
  it('appends an encoded window with the right separator', () => {
    expect(withTimeRange('/x', range)).toBe(
      '/x?from=2026-11-08T10%3A00%3A00Z&to=2026-11-08T11%3A00%3A00%2B01%3A00')
    expect(withTimeRange('/x?database=a', range)).toMatch(/^\/x\?database=a&from=/)
  })
  it('passes a missing base or range through', () => {
    expect(withTimeRange(null, range)).toBeNull()
    expect(withTimeRange('/x', null)).toBe('/x')
  })
})
