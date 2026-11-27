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
    expect(result.current.error).toBe('boom (500)')
    expect(result.current.rows).toEqual([{ id: 1 }])
    expect(result.current.hasMore).toBe(true)
    expect(result.current.loading).toBe(false)
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBeNull()
    expect(result.current.rows).toEqual([{ id: 1 }, { id: 2 }])
  })

  it('reports a status with no reason phrase', async () => {
    stubFetch({ [next]: { status: 502 } })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBe('Request failed (502)')
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

// dashboard-05: a failed next page shows the server message, and an
// expired session reads as one.
describe('useCursorPages error messages', () => {
  const url = '/api/v1/actions'
  const next = '/api/v1/actions?cursor=c1'
  const first = { actions: [{ id: 1 }], next_cursor: 'c1' }
  const pages = () => renderHook(() => useCursorPages(first, url, 'actions'))

  it('shows the server message for a 404', async () => {
    stubFetch({ [next]: { status: 404, json: { error: 'cursor expired' } } })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBe('cursor expired (404)')
    expect(result.current.rows).toEqual([{ id: 1 }])
  })

  it('names the status line for an HTML error page', async () => {
    stubFetch({ [next]: { status: 504, statusText: 'Gateway Timeout', text: '<html/>' } })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBe('Request failed (504 Gateway Timeout)')
  })

  it('reports an expired session on 401', async () => {
    stubFetch({ [next]: { status: 401 } })
    const { result } = pages()
    await act(async () => { await result.current.loadMore() })
    expect(result.current.error).toBe('session expired')
  })
})

// dashboard-01: a poll, a live event or the 30 s time-range tick refreshes
// the first page. Loaded pages must survive it: the extra pages are keyed
// on the resource (database and filters, not from/to), and the refreshed
// first page is merged in by id. Cursors are keyset positions, so the last
// cursor stays valid across refreshes.
// No concurrent access tests beyond the in-flight cases below: one hook.
describe('useCursorPages across refreshes', () => {
  const ids = rows => rows.map(r => r.id)
  const page = (list, cursor = '') => ({ findings: list.map(id => ({ id })), next_cursor: cursor })
  function hook(url, first) {
    return renderHook(({ u, p }) => useCursorPages(p, u, 'findings'), {
      initialProps: { u: url, p: first },
    })
  }

  it('keeps loaded pages and their cursor when the same list refreshes', async () => {
    const url = '/api/v1/findings?status=open'
    const fetch = stubFetch({
      [`${url}&cursor=c1`]: { json: page([2], 'c2') },
      [`${url}&cursor=c2`]: { json: page([3]) },
    })
    const { result, rerender } = hook(url, page([1], 'c1'))
    await act(async () => { await result.current.loadMore() })
    rerender({ u: url, p: page([1], 'c1') })
    expect(ids(result.current.rows)).toEqual([1, 2])
    expect(result.current.hasMore).toBe(true)
    await act(async () => { await result.current.loadMore() })
    expect(ids(result.current.rows)).toEqual([1, 2, 3])
    expect(fetch.mock.calls.map(c => c[0])).toEqual([`${url}&cursor=c1`, `${url}&cursor=c2`])
  })

  it('keeps loaded pages when only the time window moves (the 30 s tick)', async () => {
    const iso = ms => new Date(ms).toISOString()
    const at = t => `/api/v1/findings?status=resolved&from=${iso(t)}&to=${iso(t + 86400000)}`
    stubFetch({ '/api/v1/findings': { json: page([2], 'c2') } })
    const { result, rerender } = hook(at(1000), page([1], 'c1'))
    await act(async () => { await result.current.loadMore() })
    rerender({ u: at(31000), p: page([1], 'c1') })
    expect(ids(result.current.rows)).toEqual([1, 2])
    expect(result.current.hasMore).toBe(true)
  })

  it('resets on a database switch and pages the new database from its cursor', async () => {
    const a = '/api/v1/findings?status=open&database=a'
    const b = '/api/v1/findings?status=open&database=b'
    const fetch = stubFetch({
      [`${a}&cursor=a1`]: { json: page([2], 'a2') },
      [`${b}&cursor=b1`]: { json: page([20]) },
    })
    const { result, rerender } = hook(a, page([1], 'a1'))
    await act(async () => { await result.current.loadMore() })
    expect(ids(result.current.rows)).toEqual([1, 2])
    rerender({ u: b, p: page([10], 'b1') })
    expect(ids(result.current.rows)).toEqual([10])
    expect(result.current.hasMore).toBe(true)
    await act(async () => { await result.current.loadMore() })
    expect(ids(result.current.rows)).toEqual([10, 20])
    expect(fetch.mock.calls.at(-1)[0]).toBe(`${b}&cursor=b1`)
  })

  it('resets when a filter changes', async () => {
    const open = '/api/v1/findings?status=open'
    stubFetch({ [`${open}&cursor=c1`]: { json: page([2]) } })
    const { result, rerender } = hook(open, page([1], 'c1'))
    await act(async () => { await result.current.loadMore() })
    rerender({ u: '/api/v1/findings?status=open&severity=critical', p: page([7]) })
    expect(ids(result.current.rows)).toEqual([7])
    expect(result.current.hasMore).toBe(false)
  })

  it('resets when the time range length changes (24 h to 1 h)', async () => {
    const iso = ms => encodeURIComponent(new Date(ms).toISOString())
    const win = (t, len) => `/api/v1/actions?from=${iso(t)}&to=${iso(t + len)}`
    stubFetch({ '/api/v1/actions': { json: { actions: [{ id: 2 }] } } })
    const { result, rerender } = renderHook(({ u, p }) => useCursorPages(p, u, 'actions'), {
      initialProps: { u: win(0, 86400000), p: { actions: [{ id: 1 }], next_cursor: 'c1' } },
    })
    await act(async () => { await result.current.loadMore() })
    expect(ids(result.current.rows)).toEqual([1, 2])
    rerender({ u: win(0, 3600000), p: { actions: [{ id: 1 }], next_cursor: '' } })
    expect(ids(result.current.rows)).toEqual([1])
    expect(result.current.hasMore).toBe(false)
  })

  it('merges new head rows without duplicates and keeps rows pushed off page 1', async () => {
    const url = '/api/v1/findings?status=open'
    stubFetch({ [`${url}&cursor=c3`]: { json: page([4, 5, 6], 'c6') } })
    const { result, rerender } = hook(url, page([1, 2, 3], 'c3'))
    await act(async () => { await result.current.loadMore() })
    // Two new rows arrive at the head: 2 and 3 fall off page 1.
    rerender({ u: url, p: page([11, 10, 1], 'c1') })
    const got = ids(result.current.rows)
    expect(got).toEqual([11, 10, 1, 2, 3, 4, 5, 6])
    expect(new Set(got).size).toBe(got.length)
    expect(result.current.hasMore).toBe(true)
  })

  it('shows no duplicate ids when the refreshed page overlaps the loaded pages', async () => {
    const url = '/api/v1/findings?status=open'
    stubFetch({ [`${url}&cursor=c3`]: { json: page([4, 5, 6], 'c6') } })
    const { result, rerender } = hook(url, page([1, 2, 3], 'c3'))
    await act(async () => { await result.current.loadMore() })
    // Row 1 was resolved: the page slides down over row 4 of page 2.
    rerender({ u: url, p: page([2, 3, 4], 'c4') })
    const got = ids(result.current.rows)
    expect(got).toEqual([2, 3, 4, 5, 6])
    expect(new Set(got).size).toBe(got.length)
    // A refreshed row wins over the copy loaded earlier.
    rerender({ u: url, p: { findings: [{ id: 2 }, { id: 3 }, { id: 4, v: 'new' }],
      next_cursor: 'c4' } })
    expect(result.current.rows.filter(r => r.id === 4)).toEqual([{ id: 4, v: 'new' }])
  })

  it('keeps the loaded pages for an empty or missing refreshed first page', async () => {
    const url = '/api/v1/findings?status=open'
    stubFetch({ [`${url}&cursor=c1`]: { json: page([2], 'c2') } })
    const { result, rerender } = hook(url, page([1], 'c1'))
    await act(async () => { await result.current.loadMore() })
    rerender({ u: url, p: { next_cursor: '' } })
    expect(ids(result.current.rows)).toEqual([1, 2])
    expect(result.current.hasMore).toBe(true)
    rerender({ u: url, p: null })
    expect(ids(result.current.rows)).toEqual([1, 2])
  })

  it('keeps rows of different databases that share an id (fleet lists)', async () => {
    const url = '/api/v1/findings?status=open'
    stubFetch({ [`${url}&cursor=c1`]: { json: { findings: [
      { id: 1, database_name: 'b' }, { id: 1, database_name: 'a' }] } } })
    const { result } = hook(url, { findings: [{ id: 1, database_name: 'a' }],
      next_cursor: 'c1' })
    await act(async () => { await result.current.loadMore() })
    expect(result.current.rows).toEqual([
      { id: 1, database_name: 'a' }, { id: 1, database_name: 'b' }])
  })

  it('dedupes with a custom idOf', async () => {
    const url = '/api/v1/actions'
    stubFetch({ [`${url}?cursor=c1`]: { json: { actions: [
      { id: 7, ledger_key: 'queue:7' }, { id: 7, ledger_key: 'log:7' }] } } })
    const { result } = renderHook(() => useCursorPages(
      { actions: [{ id: 7, ledger_key: 'log:7' }], next_cursor: 'c1' }, url, 'actions',
      { idOf: r => r.ledger_key }))
    await act(async () => { await result.current.loadMore() })
    expect(result.current.rows.map(r => r.ledger_key)).toEqual(['log:7', 'queue:7'])
  })

  it('starts over when the operator switches away and back (a to b to a)', async () => {
    const a = '/api/v1/findings?status=open&database=a'
    const b = '/api/v1/findings?status=open&database=b'
    const fetch = stubFetch({
      [`${a}&cursor=a1`]: { json: page([2], 'a2') },
      [`${a}&cursor=a9`]: { json: page([8]) },
    })
    const { result, rerender } = hook(a, page([1], 'a1'))
    await act(async () => { await result.current.loadMore() })
    expect(ids(result.current.rows)).toEqual([1, 2])
    rerender({ u: b, p: page([10]) })
    rerender({ u: a, p: page([7], 'a9') })
    // Page 2 of the earlier visit is not current: only the fresh first page.
    expect(ids(result.current.rows)).toEqual([7])
    await act(async () => { await result.current.loadMore() })
    expect(ids(result.current.rows)).toEqual([7, 8])
    expect(fetch.mock.calls.at(-1)[0]).toBe(`${a}&cursor=a9`)
  })

  it('does not show a page that lands after a database switch', async () => {
    const a = '/api/v1/findings?database=a'
    const b = '/api/v1/findings?database=b'
    let release
    stubFetch({ [`${a}&cursor=a1`]: () => new Promise(r => { release = r }) })
    const { result, rerender } = hook(a, page([1], 'a1'))
    let pending
    act(() => { pending = result.current.loadMore() })
    rerender({ u: b, p: page([10]) })
    await act(async () => {
      release({ json: page([2], 'a2') })
      await pending
    })
    expect(ids(result.current.rows)).toEqual([10])
    expect(result.current.hasMore).toBe(false)
  })
})
