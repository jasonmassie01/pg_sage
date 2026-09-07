import { useState, useEffect, useCallback, useRef } from 'react'

// useAPI polls `url` at `interval` ms. It aborts in-flight requests
// when url/interval changes or the component unmounts so stale
// responses never clobber state.
export function useAPI(url, interval = 30000) {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const abortRef = useRef(null)
  const dataRef = useRef(null)
  const urlRef = useRef(null)

  const fetchData = useCallback(async () => {
    if (!url) {
      dataRef.current = null
      urlRef.current = null
      setData(null)
      setError(null)
      setLoading(false)
      return
    }
    if (urlRef.current !== url) {
      urlRef.current = url
      dataRef.current = null
      setData(null)
    }
    if (abortRef.current) {
      abortRef.current.abort()
    }
    const ctrl = new AbortController()
    abortRef.current = ctrl
    if (dataRef.current === null) {
      setLoading(true)
    }
    try {
      const res = await fetch(url, { signal: ctrl.signal })
      if (res.status === 401) {
        // Session expired mid-session: notify the app to return to the
        // login screen instead of showing a 401 banner on every polling
        // page until a full reload (FE1).
        if (typeof window !== 'undefined') {
          window.dispatchEvent(new CustomEvent('sage:auth-expired'))
        }
        throw new Error('session expired')
      }
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
      const json = await res.json()
      dataRef.current = json
      setData(json)
      setError(null)
    } catch (err) {
      if (err.name === 'AbortError') return
      setError(err.message)
    } finally {
      if (abortRef.current === ctrl) {
        setLoading(false)
        abortRef.current = null
      }
    }
  }, [url])

  useEffect(() => {
    fetchData()
    if (interval > 0 && url) {
      const id = setInterval(fetchData, interval)
      return () => {
        clearInterval(id)
        if (abortRef.current) abortRef.current.abort()
      }
    }
    return () => {
      if (abortRef.current) abortRef.current.abort()
    }
  }, [fetchData, interval, url])

  return { data, loading, error, refetch: fetchData }
}

// withTimeRange returns `base` with ?from/&to query params appended
// from the supplied TimeRange context value. Pass null for base to
// skip the fetch entirely (matches useAPI(null) behavior).
export function withTimeRange(base, range) {
  if (!base || !range) return base
  const sep = base.includes('?') ? '&' : '?'
  return (
    `${base}${sep}from=${encodeURIComponent(range.fromISO)}` +
    `&to=${encodeURIComponent(range.toISO)}`
  )
}
