import { useState, useEffect, useCallback, useRef } from 'react'

// resourceKey identifies what a URL fetches, ignoring the from/to time
// window. Moving the window (the 30 s time-range tick) refreshes the
// same resource, so its data stays visible while revalidating (G9-B05).
export function resourceKey(url) {
  if (!url) return null
  const [path, query = ''] = url.split('?')
  const params = new URLSearchParams(query)
  params.delete('from')
  params.delete('to')
  const rest = params.toString()
  return rest ? `${path}?${rest}` : path
}

// useAPI polls `url` at `interval` ms. Data is keyed by resource, so a
// different database never renders the previous database's data, and a
// new time window keeps the current data until the refresh lands.
// In-flight requests are aborted on url change and unmount.
export function useAPI(url, interval = 30000) {
  const key = resourceKey(url)
  const [state, setState] = useState({ key: null, data: null, error: null })
  const [inFlight, setInFlight] = useState(false)
  const abortRef = useRef(null)

  const fetchData = useCallback(async () => {
    if (!url) {
      setState({ key: null, data: null, error: null })
      setInFlight(false)
      return
    }
    const requestKey = resourceKey(url)
    if (abortRef.current) abortRef.current.abort()
    const ctrl = new AbortController()
    abortRef.current = ctrl
    setInFlight(true)
    try {
      const res = await fetch(url, { signal: ctrl.signal })
      // 401s are turned into sage:auth-expired by the global fetch
      // interceptor (lib/authExpiry.js).
      if (res.status === 401) throw new Error('session expired')
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
      const json = await res.json()
      setState({ key: requestKey, data: json, error: null })
    } catch (err) {
      if (err.name === 'AbortError') return
      setState(prev => ({
        key: requestKey,
        data: prev.key === requestKey ? prev.data : null,
        error: err.message,
      }))
    } finally {
      if (abortRef.current === ctrl) {
        setInFlight(false)
        abortRef.current = null
      }
    }
  }, [url])

  useEffect(() => {
    fetchData()
    const id = interval > 0 && url ? setInterval(fetchData, interval) : null
    return () => {
      if (id) clearInterval(id)
      if (abortRef.current) abortRef.current.abort()
    }
  }, [fetchData, interval, url])

  const current = state.key === key
  const data = current ? state.data : null
  const error = current ? state.error : null
  const loading = Boolean(url) &&
    (!current || (data === null && error === null && inFlight))
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
