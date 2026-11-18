import { useCallback, useEffect, useRef, useState } from 'react'
import { apiGet } from '../lib/api'
import { databaseSearchURL } from '../lib/fleetScope'

// useDatabaseSearch serves the header scope picker (ux-10b) from the fleet
// list's q and tag (ux-10a), one page at a time. A new query aborts the
// request in flight, and an answer that lands after its query changed is
// dropped, so the last query's answer always wins. Nothing is requested
// while the picker is closed. Each answer is kept with the URL it answers,
// so "loading" is simply "no answer for this URL yet".

const EMPTY = { key: null, rows: [], total: 0, next: '', error: null }

function pageOf(page) {
  const rows = Array.isArray(page?.databases)
    ? page.databases.filter(db => typeof db?.name === 'string' && db.name) : []
  return { rows, total: Number.isFinite(page?.total) ? page.total : rows.length,
    next: typeof page?.next_cursor === 'string' ? page.next_cursor : '' }
}

export function useDatabaseSearch({ open, q, tag }) {
  const url = databaseSearchURL({ q, tag })
  const [state, setState] = useState(EMPTY)
  const [moreFor, setMoreFor] = useState(null)
  const more = useRef(null)

  useEffect(() => {
    if (!open) return undefined
    const ctrl = new AbortController()
    apiGet(url, { signal: ctrl.signal })
      .then(page => {
        if (!ctrl.signal.aborted) setState({ ...pageOf(page), key: url, error: null })
      })
      .catch(err => {
        if (ctrl.signal.aborted || err?.name === 'AbortError') return
        setState({ ...EMPTY, key: url, error: err })
      })
    return () => {
      ctrl.abort()
      more.current?.abort()
    }
  }, [open, url])

  const current = state.key === url
  const cursor = current ? state.next : ''
  const loadingMore = Boolean(cursor) && moreFor === `${url}#${cursor}`
  const loadMore = useCallback(() => {
    if (!cursor || loadingMore) return
    const ctrl = new AbortController()
    more.current = ctrl
    setMoreFor(`${url}#${cursor}`)
    apiGet(databaseSearchURL({ q, tag, cursor }), { signal: ctrl.signal })
      .then(page => {
        if (ctrl.signal.aborted) return
        const got = pageOf(page)
        setState(prev => (prev.key !== url ? prev
          : { ...got, key: url, error: null, rows: [...prev.rows, ...got.rows] }))
      })
      .catch(err => {
        if (ctrl.signal.aborted || err?.name === 'AbortError') return
        setMoreFor(null)
        setState(prev => (prev.key !== url ? prev : { ...prev, next: '', error: err }))
      })
  }, [url, q, tag, cursor, loadingMore])

  return {
    rows: current ? state.rows : [], total: current ? state.total : 0, next: cursor,
    error: current ? state.error : null, loading: open && (!current || loadingMore),
    loadMore,
  }
}
