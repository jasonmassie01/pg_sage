import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { stubFetch } from '../test/fetchStub'
import { formatTotal, useCursorPages, withCursor } from './listPaging'

afterEach(() => vi.unstubAllGlobals())

// The findings and actions lists report a total capped at 1000 with
// total_capped (perf v1.8.3): past the cap the count is "1000+", never a
// bare 1000 that reads as exact.
describe('formatTotal', () => {
  it('marks a capped total', () => {
    expect(formatTotal({ total: 1000, total_capped: true })).toBe('1000+')
  })
  it('shows an exact total as is', () => {
    expect(formatTotal({ total: 1000, total_capped: false })).toBe('1000')
    expect(formatTotal({ total: 37 })).toBe('37')
  })
  it('treats a missing response or total as zero', () => {
    expect(formatTotal(null)).toBe('0')
    expect(formatTotal({})).toBe('0')
  })
})

describe('withCursor', () => {
  it('adds the cursor to a URL with or without a query', () => {
    expect(withCursor('/api/v1/findings?status=open', 'a b'))
      .toBe('/api/v1/findings?status=open&cursor=a%20b')
    expect(withCursor('/api/v1/actions', 'c1')).toBe('/api/v1/actions?cursor=c1')
  })
  it('replaces an offset (the cursor pages, the offset is capped)', () => {
    expect(withCursor('/api/v1/actions?offset=50&limit=50', 'c2'))
      .toBe('/api/v1/actions?limit=50&cursor=c2')
  })
})

// useCursorPages through the real hook and real Response objects.
describe('useCursorPages', () => {
  const url = '/api/v1/findings?status=open'
  const next = '/api/v1/findings?status=open&cursor=c1'
  const after = '/api/v1/findings?status=open&cursor=c2'
  const first = { findings: [{ id: 1 }], next_cursor: 'c1' }

  function pages(firstPage = first) {
    return renderHook(({ page }) => useCursorPages(page, url, 'findings'), {
      initialProps: { page: firstPage },
    })
  }

  it('appends the next page and follows its cursor', async () => {
    const fetch = stubFetch({
      [next]: { json: { findings: [{ id: 2 }], next_cursor: 'c2' } },
      [after]: { json: { findings: [{ id: 3 }] } },
    })
    const { result } = pages()
    expect(result.current.rows).toEqual([{ id: 1 }])
    expect(result.current.hasMore).toBe(true)
    await act(async () => { await result.current.loadMore() })
    expect(result.current.rows).toEqual([{ id: 1 }, { id: 2 }])
    expect(result.current.hasMore).toBe(true)
    await act(async () => { await result.current.loadMore() })
    expect(result.current.rows.map(r => r.id)).toEqual([1, 2, 3])
    expect(result.current.hasMore).toBe(false)
    expect(fetch.mock.calls.map(c => c[0])).toEqual([next, after])
  })

  it('sets error on a 500 and clears it on the next success', async () => {
    stubFetch({
      [next]: [
        { status: 500, statusText: 'Internal Server Error', json: { error: 'boom' } },
        { json: { findings: [{ id: 2 }] } },
      ],
    })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBe('500 Internal Server Error')
    expect(result.current.rows).toEqual([{ id: 1 }])
    expect(result.current.hasMore).toBe(true)
    expect(result.current.loading).toBe(false)
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBeNull()
    expect(result.current.rows).toEqual([{ id: 1 }, { id: 2 }])
  })

  it('reports a status with no reason phrase without a trailing space', async () => {
    stubFetch({ [next]: { status: 502 } })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBe('502')
  })

  it('reports a network failure', async () => {
    stubFetch({ [next]: () => Promise.reject(new TypeError('Failed to fetch')) })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBe('Failed to fetch')
  })

  it('does nothing without a cursor or a first page', async () => {
    const fetch = stubFetch({})
    const { result, rerender } = pages({ findings: [{ id: 1 }], next_cursor: '' })
    expect(result.current.hasMore).toBe(false)
    await act(async () => { await result.current.loadMore() })
    rerender({ page: null })
    expect(result.current.rows).toEqual([])
    expect(result.current.hasMore).toBe(false)
    await act(async () => { await result.current.loadMore() })
    expect(fetch).not.toHaveBeenCalled()
  })

  it('treats a page without rows as an empty page', async () => {
    stubFetch({ [next]: { json: { next_cursor: '' } } })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.rows).toEqual([{ id: 1 }])
    expect(result.current.hasMore).toBe(false)
    expect(result.current.error).toBeNull()
  })

  it('drops loaded pages when the first page changes', async () => {
    stubFetch({ [next]: { json: { findings: [{ id: 2 }], next_cursor: 'c2' } } })
    const { result, rerender } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.rows).toHaveLength(2)
    rerender({ page: { findings: [{ id: 9 }], next_cursor: 'c1' } })
    expect(result.current.rows).toEqual([{ id: 9 }])
    expect(result.current.hasMore).toBe(true)
  })

  it('ignores a second loadMore while one is in flight', async () => {
    let release
    const fetch = stubFetch({ [next]: () => new Promise(r => { release = r }) })
    const { result } = pages()
    let firstCall
    act(() => { firstCall = result.current.loadMore() })
    expect(result.current.loading).toBe(true)
    await act(async () => { await result.current.loadMore() })
    expect(fetch).toHaveBeenCalledTimes(1)
    await act(async () => {
      release({ json: { findings: [{ id: 2 }] } })
      await firstCall
    })
    expect(result.current.rows).toEqual([{ id: 1 }, { id: 2 }])
    expect(result.current.loading).toBe(false)
  })
})
