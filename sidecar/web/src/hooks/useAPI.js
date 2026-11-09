import { useState, useEffect, useCallback, useRef } from 'react'
import { apiGet } from '../lib/api'
import { resourceKey } from '../lib/resourceKey'
import { useVisibleWake } from './useVisibleWake'

export { resourceKey }

// MAX_BACKOFF_MS caps the wait between polls of a failing endpoint.
export const MAX_BACKOFF_MS = 5 * 60 * 1000

// pollDelay is the wait before the next poll after `failures` failures in
// a row: the interval after none or one, then 2x, 4x and so on, capped at
// MAX_BACKOFF_MS but never shorter than the interval itself.
export function pollDelay(interval, failures) {
  if (failures <= 1) return interval
  return Math.max(interval, Math.min(interval * 2 ** (failures - 1), MAX_BACKOFF_MS))
}

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
// shown again; failures in a row back off (pollDelay). refetch() always
// fetches at once.
export function useAPI(url, interval = 30000) {
  const key = resourceKey(url)
  const [state, setState] = useState(EMPTY)
  const [inFlight, setInFlight] = useState(false)
  const abortRef = useRef(null)
  const poll = useRef({ timer: null, failures: 0, paused: false, schedule: null })
  const intervalRef = useRef(interval)
  useEffect(() => { intervalRef.current = interval }, [interval])
  const wake = useVisibleWake()

  const fetchData = useCallback(async () => {
    if (!url) {
      setState(EMPTY)
      setInFlight(false)
      return
    }
    const requestKey = resourceKey(url)
    if (abortRef.current) abortRef.current.abort()
    const ctrl = new AbortController()
    abortRef.current = ctrl
    setInFlight(true)
    try {
      // 401s are turned into sage:auth-expired by the global fetch
      // interceptor (lib/authExpiry.js).
      const json = await apiGet(url, { signal: ctrl.signal })
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
  }, [url])

  const polling = interval > 0 && Boolean(url)
  useEffect(() => {
    const p = poll.current
    p.failures = 0
    p.paused = false
    p.schedule = polling ? () => {
      clearTimeout(p.timer)
      p.timer = setTimeout(tick, pollDelay(intervalRef.current, p.failures))
    } : null
    function tick() {
      p.timer = null
      if (document.hidden) {
        p.paused = true
        return
      }
      // Scheduled before the fetch too, so a request that never answers
      // is superseded by the next poll instead of stopping the polling.
      p.schedule()
      fetchData()
    }
    fetchData()
    p.schedule?.()
    return () => {
      clearTimeout(p.timer)
      p.timer = null
      p.schedule = null
      if (abortRef.current) abortRef.current.abort()
    }
  }, [fetchData, polling])

  // A poll skipped while hidden runs once when the tab is shown, unless the
  // same render already started a fetch (a time-range tick moved the url).
  useEffect(() => {
    const p = poll.current
    if (!p.paused || document.hidden) return
    p.paused = false
    if (!abortRef.current) fetchData()
  }, [wake, fetchData])

  return { ...visibleState(state, key, url, inFlight), refetch: fetchData }
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
