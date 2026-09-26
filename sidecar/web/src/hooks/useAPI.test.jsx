import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useAPI, resourceKey } from './useAPI'

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
