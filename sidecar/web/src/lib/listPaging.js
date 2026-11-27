import { useCallback, useEffect, useRef, useState } from 'react'
import { apiGet } from './api'
import { resourceKey } from './resourceKey'

// The findings and actions lists (perf v1.8.3) report `total` capped at
// 1000 with `total_capped`, and page with an opaque `next_cursor`.

// formatTotal renders a list total: "1000+" past the cap, never a bare
// 1000 that reads as exact.
export function formatTotal(data) {
  const total = Number(data?.total) || 0
  return data?.total_capped ? `${total}+` : String(total)
}

// withCursor is url asking for the page after cursor. An offset is
// dropped: the cursor pages, the offset is capped server-side.
export function withCursor(url, cursor) {
  const [path, query = ''] = url.split('?')
  const params = new URLSearchParams(query)
  params.delete('offset')
  params.delete('cursor')
  const rest = params.toString()
  const sep = rest ? `${rest}&` : ''
  return `${path}?${sep}cursor=${encodeURIComponent(cursor)}`
}

// pagesKey names the list a URL pages: its resource (database and
// filters, from useAPI's resourceKey) plus the length of its time window.
// The 30 s tick moves from/to by the same amount and keeps the key; picking
// another range (24 h to 1 h) changes it, so pages of the old window go.
export function pagesKey(url) {
  const resource = resourceKey(url)
  if (!resource) return null
  const params = new URLSearchParams(url.split('?')[1] || '')
  const span = Date.parse(params.get('to')) - Date.parse(params.get('from'))
  return Number.isFinite(span) ? `${resource}#${span}` : resource
}

// rowIdentity is a list row's id within its database: fleet lists hold
// rows of several databases whose ids overlap. A row without an id is
// known by its content.
function rowIdentity(row) {
  if (row?.id === undefined || row?.id === null) return `row:${JSON.stringify(row)}`
  return JSON.stringify([row.database_name ?? row.database ?? '', row.id])
}

function sameIds(a, b, idOf) {
  return a.length === b.length && a.every((row, i) => idOf(row) === idOf(b[i]))
}

// mergeRows concatenates lists, dropping a row whose id was already seen:
// the first occurrence (the freshest copy) wins.
function mergeRows(lists, idOf) {
  const seen = new Set()
  const out = []
  for (const list of lists) {
    for (const row of list) {
      const id = idOf(row)
      if (seen.has(id)) continue
      seen.add(id)
      out.push(row)
    }
  }
  return out
}

// tailAfter is what stays below a refreshed first page. The rows shown
// before (old first page, then the loaded pages) are in list order; the
// ones after the new page's last row were pushed off it and stay. The ones
// before it that the new page lacks left the list (a resolved finding) and
// go. If the new page's last row was not shown before, every row shown is
// older than the whole new page and stays.
function tailAfter(shown, fresh, idOf) {
  const last = fresh.length ? idOf(fresh[fresh.length - 1]) : undefined
  const at = last === undefined ? -1 : shown.findIndex(r => idOf(r) === last)
  return at < 0 ? shown : shown.slice(at + 1)
}

const NO_ROWS = []
const NO_PAGES = { key: null, base: NO_ROWS, tail: NO_ROWS, cursor: '' }

// useCursorPages shows firstPage (the useAPI response for url) followed by
// the pages loadMore fetched. Loaded pages are kept per list (pagesKey):
// a refresh of the first page (a poll, a live event, the time tick) keeps
// them and their cursor, and its rows are merged in by idOf with no
// duplicates. List cursors are keyset positions, so the last cursor stays
// valid. Another database, filter or range length starts over.
export function useCursorPages(firstPage, url, key, { idOf = rowIdentity } = {}) {
  const listKey = pagesKey(url)
  const [pages, setPages] = useState(NO_PAGES)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const listRef = useRef(listKey)
  useEffect(() => { listRef.current = listKey }, [listKey])
  useEffect(() => { setError(null) }, [firstPage])
  const live = listKey !== null && pages.key === listKey
  const head = firstPage?.[key] || NO_ROWS
  if (pages.key !== null && pages.key !== listKey) {
    // Another list: drop the old one's pages, so coming back starts over.
    setPages(NO_PAGES)
  } else if (live && !sameIds(pages.base, head, idOf)) {
    // The first page refreshed with other rows: refold what was below it.
    const shown = mergeRows([pages.base, pages.tail], idOf)
    setPages({ ...pages, base: head, tail: tailAfter(shown, head, idOf) })
  }
  const cursor = live ? pages.cursor : (firstPage?.next_cursor || '')
  const loadMore = useCallback(async () => {
    if (!cursor || loading) return
    setLoading(true)
    setError(null)
    try {
      const page = await apiGet(withCursor(url, cursor))
      // A page that lands after a switch to another list is dropped.
      if (listRef.current !== listKey) return
      setPages(prev => ({
        key: listKey,
        base: prev.key === listKey ? prev.base : head,
        tail: [...(prev.key === listKey ? prev.tail : []), ...(page?.[key] || [])],
        cursor: page?.next_cursor || '',
      }))
    } catch (err) {
      setError(err.message || 'failed to load the next page')
    } finally {
      setLoading(false)
    }
  }, [cursor, loading, url, key, head, listKey])
  const rows = live ? mergeRows([head, pages.tail], idOf) : head
  return { rows, hasMore: Boolean(cursor), loadMore, loading, error }
}
