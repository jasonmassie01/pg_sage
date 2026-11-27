import { useState, useCallback, useRef } from 'react'
import { apiGet } from '../lib/api'
import { resourceKey } from '../lib/resourceKey'
import { idlePoll, usePolling } from './usePolling'

export { resourceKey }

// useAPI polls `url` at `interval` ms. Data is keyed by resource, so a
// different database never renders the previous database's data, and a
// new time window keeps the current data until the refresh lands.
// In-flight requests are aborted on url change and unmount. updatedAt is
// the time (ms) of the resource's last successful response, or null;
// failedAt is the time of its latest failed request since then, or null.
// stale is true while a failed refresh leaves the last good data on screen.
// error is the message the operator reads (the server's own {error} when it
// sent one); errorStatus is the HTTP status, or null for a network failure.
//
// Polling is bounded (dashboard-08): a poll that falls due while
// document.hidden is skipped and the hook fetches once when the tab is
// shown again; failures in a row back off (usePolling). refetch() always
// fetches at once.
//
// url may also be a function returning the url (or null), called when each
// request starts and on render for the resource key (dashboard-07): a
// windowed url is then computed at fetch time instead of moving, and
// refetching, on a render. Pass a function whose identity changes only when
// the resource or window length does.
export function useAPI(url, interval = 30000) {
  const resolveURL = useCallback(() => (typeof url === 'function' ? url() : url), [url])
  const renderURL = resolveURL()
  const key = resourceKey(renderURL)
  const [state, setState] = useState(EMPTY)
  const [inFlight, setInFlight] = useState(false)
  const abortRef = useRef(null)
  const poll = useRef(idlePoll())

  const fetchData = useCallback(async () => {
    const target = resolveURL()
    if (!target) {
      setState(EMPTY)
      setInFlight(false)
      return
    }
    const requestKey = resourceKey(target)
    if (abortRef.current) abortRef.current.abort()
    const ctrl = new AbortController()
    abortRef.current = ctrl
    setInFlight(true)
    try {
      // 401s are turned into sage:auth-expired by the global fetch
      // interceptor (lib/authExpiry.js).
      const json = await apiGet(target, { signal: ctrl.signal })
      poll.current.failures = 0
      setState({ ...EMPTY, key: requestKey, data: json, updatedAt: Date.now() })
    } catch (err) {
      if (err.name === 'AbortError') return
      poll.current.failures += 1
      setState(prev => failedState(prev, requestKey, err))
    } finally {
      if (abortRef.current === ctrl) {
        setInFlight(false)
        abortRef.current = null
      }
    }
    // The next poll counts from this answer, with any backoff it earned.
    poll.current.schedule?.()
  }, [resolveURL])

  usePolling(poll, fetchData, {
    key, interval, active: interval > 0 && Boolean(renderURL), abortRef,
  })

  return { ...visibleState(state, key, renderURL, inFlight), refetch: fetchData }
}

const EMPTY = {
  key: null, data: null, error: null, errorStatus: null, updatedAt: null, failedAt: null,
}

// failedState keeps the resource's last good data and its time when a
// refresh of the same resource fails; a different resource starts empty.
function failedState(prev, key, err) {
  const same = prev.key === key
  return {
    key,
    data: same ? prev.data : null,
    error: err.message,
    errorStatus: Number.isInteger(err.status) ? err.status : null,
    updatedAt: same ? prev.updatedAt : null,
    failedAt: Date.now(),
  }
}

// visibleState shows only state that belongs to the current resource.
function visibleState(state, key, url, inFlight) {
  const current = state.key === key
  const data = current ? state.data : null
  const error = current ? state.error : null
  const errorStatus = current ? state.errorStatus : null
  const updatedAt = current ? state.updatedAt : null
  const failedAt = current ? state.failedAt : null
  const loading = Boolean(url) &&
    (!current || (data === null && error === null && inFlight))
  const stale = Boolean(error) && data !== null
  return { data, loading, error, errorStatus, updatedAt, failedAt, stale }
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
