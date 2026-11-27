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
    expect(result.current.error).toBe('Request failed (503 Service Unavailable)')
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

// dashboard-04: updatedAt is the time of the last successful response, so
// the header can say how old the data on screen is.
describe('useAPI updatedAt', () => {
  it('is null before the first response and stamped by a success', async () => {
    stubFetch({ '/api/v1/databases': { json: { databases: [] } } })
    const before = Date.now()
    const { result } = renderHook(() => useAPI('/api/v1/databases', 0))
    expect(result.current.updatedAt).toBeNull()
    await waitFor(() => expect(result.current.data).toEqual({ databases: [] }))
    expect(typeof result.current.updatedAt).toBe('number')
    expect(result.current.updatedAt).toBeGreaterThanOrEqual(before)
    expect(result.current.updatedAt).toBeLessThanOrEqual(Date.now())
  })

  it('keeps the last success time when a refresh fails', async () => {
    stubFetch({
      '/api/v1/databases': [
        { json: { n: 1 } },
        { status: 502, statusText: 'Bad Gateway', text: 'proxy' },
      ],
    })
    const { result } = renderHook(() => useAPI('/api/v1/databases', 0))
    await waitFor(() => expect(result.current.data).toEqual({ n: 1 }))
    const stamped = result.current.updatedAt
    expect(typeof stamped).toBe('number')
    await act(async () => { await result.current.refetch() })
    expect(result.current.error).toBe('Request failed (502 Bad Gateway)')
    expect(result.current.updatedAt).toBe(stamped)
  })

  it('stays null when the first response fails', async () => {
    stubFetch({ '/api/v1/databases': { status: 500, statusText: 'Internal Server Error' } })
    const { result } = renderHook(() => useAPI('/api/v1/databases', 0))
    await waitFor(() => expect(result.current.error)
      .toBe('Request failed (500 Internal Server Error)'))
    expect(result.current.updatedAt).toBeNull()
  })

  it('moves forward on each successful poll', async () => {
    vi.useFakeTimers()
    try {
      vi.setSystemTime(new Date('2026-11-08T14:00:00Z'))
      stubFetch({ '/api/v1/databases': { json: { ok: true } } })
      const { result } = renderHook(() => useAPI('/api/v1/databases', 30000))
      await act(async () => { await vi.advanceTimersByTimeAsync(0) })
      expect(result.current.updatedAt).toBe(Date.parse('2026-11-08T14:00:00Z'))
      await act(async () => { await vi.advanceTimersByTimeAsync(30000) })
      expect(result.current.updatedAt).toBe(Date.parse('2026-11-08T14:00:30Z'))
    } finally {
      vi.useRealTimers()
    }
  })

  it('does not carry the time of one resource over to another', async () => {
    stubFetch({
      '/api/v1/actions?database=a': { json: { db: 'a' } },
      '/api/v1/actions?database=b': { status: 503, statusText: 'Service Unavailable' },
    })
    const { result, rerender } = renderHook(({ url }) => useAPI(url, 0), {
      initialProps: { url: '/api/v1/actions?database=a' },
    })
    await waitFor(() => expect(result.current.data).toEqual({ db: 'a' }))
    expect(result.current.updatedAt).not.toBeNull()
    rerender({ url: '/api/v1/actions?database=b' })
    expect(result.current.updatedAt).toBeNull()
    await waitFor(() => expect(result.current.error)
      .toBe('Request failed (503 Service Unavailable)'))
    expect(result.current.updatedAt).toBeNull()
  })

  it('is null for a null url', () => {
    const { result } = renderHook(() => useAPI(null, 0))
    expect(result.current.updatedAt).toBeNull()
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

// dashboard-05: the operator sees the server's {"error": msg}, not a bare
// status, and callers can branch on the status through errorStatus.
describe('useAPI error messages', () => {
  it('shows the server message with its status', async () => {
    stubFetch({ '/api/v1/databases/x': { status: 404, json: { error: 'database not found' } } })
    const { result } = renderHook(() => useAPI('/api/v1/databases/x', 0))
    await waitFor(() => expect(result.current.error).toBe('database not found (404)'))
    expect(result.current.errorStatus).toBe(404)
    expect(result.current.data).toBeNull()
    expect(result.current.loading).toBe(false)
  })

  it('names the status line for an HTML error page', async () => {
    stubFetch({ '/api/v1/actions': { status: 502, statusText: 'Bad Gateway', text: '<html/>' } })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.error).toBe('Request failed (502 Bad Gateway)'))
    expect(result.current.errorStatus).toBe(502)
  })

  it('names the bare status when HTTP/2 sends no reason phrase', async () => {
    stubFetch({ '/api/v1/actions': { status: 500 } })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.error).toBe('Request failed (500)'))
    expect(result.current.errorStatus).toBe(500)
  })

  it('has a null errorStatus for a network failure', async () => {
    stubFetch({ '/api/v1/actions': () => Promise.reject(new TypeError('Failed to fetch')) })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.error).toBe('Failed to fetch'))
    expect(result.current.errorStatus).toBeNull()
  })

  it('keeps 401 as an expired session with its status', async () => {
    stubFetch({ '/api/v1/actions': { status: 401, json: { error: 'unauthorized' } } })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.error).toBe('session expired'))
    expect(result.current.errorStatus).toBe(401)
  })

  it('clears error and errorStatus on the next success', async () => {
    stubFetch({
      '/api/v1/actions': [
        { status: 404, json: { error: 'gone' } },
        { json: { n: 2 } },
      ],
    })
    const { result } = renderHook(() => useAPI('/api/v1/actions', 0))
    await waitFor(() => expect(result.current.errorStatus).toBe(404))
    await act(async () => { await result.current.refetch() })
    expect(result.current.data).toEqual({ n: 2 })
    expect(result.current.error).toBeNull()
    expect(result.current.errorStatus).toBeNull()
  })

  it('does not show one resource\'s errorStatus for another', async () => {
    stubFetch({
      '/api/v1/actions?database=a': { status: 404, json: { error: 'no a' } },
      '/api/v1/actions?database=b': () => new Promise(() => {}),
    })
    const { result, rerender } = renderHook(({ url }) => useAPI(url, 0), {
      initialProps: { url: '/api/v1/actions?database=a' },
    })
    await waitFor(() => expect(result.current.errorStatus).toBe(404))
    rerender({ url: '/api/v1/actions?database=b' })
    expect(result.current.error).toBeNull()
    expect(result.current.errorStatus).toBeNull()
  })

  it('has a null errorStatus for a null url', () => {
    const { result } = renderHook(() => useAPI(null, 0))
    expect(result.current.errorStatus).toBeNull()
  })
})

// dashboard-01: a failed refresh keeps the data and says so. stale is true
// only while showing kept data under an error; failedAt is when the latest
// refresh failed. No concurrent access tests: one hook, single-threaded.
describe('useAPI stale', () => {
  it('marks kept data stale after a failed refresh and clears it on success', async () => {
    vi.useFakeTimers()
    try {
      vi.setSystemTime(new Date(2026, 10, 8, 14, 2, 0))
      stubFetch({
        '/api/v1/cases': [
          { json: { n: 1 } },
          { status: 502, json: { error: 'upstream down' } },
          { json: { n: 2 } },
        ],
      })
      const { result } = renderHook(() => useAPI('/api/v1/cases', 180000))
      await act(async () => { await vi.advanceTimersByTimeAsync(0) })
      expect(result.current.stale).toBe(false)
      expect(result.current.failedAt).toBeNull()
      const loadedAt = result.current.updatedAt
      expect(loadedAt).toBe(new Date(2026, 10, 8, 14, 2, 0).getTime())
      await act(async () => { await vi.advanceTimersByTimeAsync(180000) })
      expect(result.current.data).toEqual({ n: 1 })
      expect(result.current.error).toBe('upstream down (502)')
      expect(result.current.stale).toBe(true)
      expect(result.current.failedAt).toBe(new Date(2026, 10, 8, 14, 5, 0).getTime())
      expect(result.current.updatedAt).toBe(loadedAt)
      expect(result.current.loading).toBe(false)
      await act(async () => { await vi.advanceTimersByTimeAsync(180000) })
      expect(result.current.data).toEqual({ n: 2 })
      expect(result.current.stale).toBe(false)
      expect(result.current.error).toBeNull()
      expect(result.current.failedAt).toBeNull()
    } finally {
      vi.useRealTimers()
    }
  })

  it('is not stale when the first response fails (there is no data to keep)', async () => {
    stubFetch({ '/api/v1/cases': { status: 503, json: { error: 'no db' } } })
    const { result } = renderHook(() => useAPI('/api/v1/cases', 0))
    await waitFor(() => expect(result.current.error).toBe('no db (503)'))
    expect(result.current.stale).toBe(false)
    expect(result.current.data).toBeNull()
    expect(typeof result.current.failedAt).toBe('number')
  })

  it('does not carry staleness to another resource', async () => {
    stubFetch({
      '/api/v1/cases?database=a': [{ json: { db: 'a' } }, { status: 502 }],
      '/api/v1/cases?database=b': () => new Promise(() => {}),
    })
    const { result, rerender } = renderHook(({ url }) => useAPI(url, 0), {
      initialProps: { url: '/api/v1/cases?database=a' },
    })
    await waitFor(() => expect(result.current.data).toEqual({ db: 'a' }))
    await act(async () => { await result.current.refetch() })
    expect(result.current.stale).toBe(true)
    rerender({ url: '/api/v1/cases?database=b' })
    expect(result.current.stale).toBe(false)
    expect(result.current.data).toBeNull()
    expect(result.current.failedAt).toBeNull()
  })

  it('is not stale for a null url', () => {
    const { result } = renderHook(() => useAPI(null, 0))
    expect(result.current.stale).toBe(false)
    expect(result.current.failedAt).toBeNull()
  })
})
