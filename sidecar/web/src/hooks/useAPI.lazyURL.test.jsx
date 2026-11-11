import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useAPI } from './useAPI'
import { stubFetch } from '../test/fetchStub'

// dashboard-07: useAPI also takes a function that returns the url. It is
// called when each request starts, so a windowed url can be computed at
// fetch time instead of moving (and refetching) on every render.
// No concurrent access tests: one hook, single-threaded fake timers.

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-09T10:00:00Z'))
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const advance = ms => act(async () => { await vi.advanceTimersByTimeAsync(ms) })

describe('useAPI with a url function', () => {
  it('builds the url when each poll starts', async () => {
    const fetch = stubFetch({ '/api/v1/x': { json: { ok: 1 } } })
    const build = () => `/api/v1/x?to=${Date.now()}`
    const start = Date.now()
    const { result } = renderHook(() => useAPI(build, 60000))
    await advance(0)
    expect(fetch.mock.calls.map(c => c[0])).toEqual([`/api/v1/x?to=${start}`])
    expect(result.current.data).toEqual({ ok: 1 })
    await advance(60000)
    expect(fetch.mock.calls.map(c => c[0]))
      .toEqual([`/api/v1/x?to=${start}`, `/api/v1/x?to=${start + 60000}`])
    expect(result.current.data).toEqual({ ok: 1 })
    expect(result.current.loading).toBe(false)
  })

  it('does not refetch when re-rendered with the same function', async () => {
    const fetch = stubFetch({ '/api/v1/x': { json: { ok: 1 } } })
    const build = () => '/api/v1/x?to=a'
    const { rerender } = renderHook(({ url }) => useAPI(url, 60000), {
      initialProps: { url: build },
    })
    await advance(0)
    rerender({ url: build })
    await advance(30000)
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it('refetches at once for a new function and keeps the same resource data', async () => {
    const fetch = stubFetch({ '/api/v1/x': [{ json: { n: 1 } }, { json: { n: 2 } }] })
    const { result, rerender } = renderHook(({ url }) => useAPI(url, 60000), {
      initialProps: { url: () => '/api/v1/x?from=1&to=2' },
    })
    await advance(0)
    expect(result.current.data).toEqual({ n: 1 })
    rerender({ url: () => '/api/v1/x?from=3&to=4' })
    // Same resource, new window: the old data stays while it revalidates.
    expect(result.current.data).toEqual({ n: 1 })
    await advance(0)
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(fetch.mock.calls[1][0]).toBe('/api/v1/x?from=3&to=4')
    expect(result.current.data).toEqual({ n: 2 })
  })

  it('fetches nothing for a function that returns null', async () => {
    const fetch = stubFetch({})
    const { result } = renderHook(() => useAPI(() => null, 60000))
    await advance(60000)
    expect(fetch).not.toHaveBeenCalled()
    expect(result.current).toMatchObject({ data: null, error: null, loading: false })
  })
})
