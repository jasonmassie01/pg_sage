import { useCallback, useEffect, useState } from 'react'

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

// useCursorPages appends the pages after firstPage (the useAPI response
// for url) to its rows. Extra pages reset when the first page changes
// (a refetch, a new filter or database).
export function useCursorPages(firstPage, url, key) {
  const [extra, setExtra] = useState({ base: null, rows: [], cursor: '' })
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const current = extra.base === firstPage ? extra : null
  useEffect(() => { setError(null) }, [firstPage])
  const cursor = current ? current.cursor : (firstPage?.next_cursor || '')
  const loadMore = useCallback(async () => {
    if (!cursor || loading) return
    setLoading(true)
    setError(null)
    try {
      const res = await fetch(withCursor(url, cursor))
      if (!res.ok) throw new Error(`${res.status} ${res.statusText || ''}`.trim())
      const page = await res.json()
      setExtra(prev => ({
        base: firstPage,
        rows: [...(prev.base === firstPage ? prev.rows : []), ...(page?.[key] || [])],
        cursor: page?.next_cursor || '',
      }))
    } catch (err) {
      setError(err.message || 'failed to load the next page')
    } finally {
      setLoading(false)
    }
  }, [cursor, loading, url, key, firstPage])
  const rows = [...(firstPage?.[key] || []), ...(current ? current.rows : [])]
  return { rows, hasMore: Boolean(cursor), loadMore, loading, error }
}
