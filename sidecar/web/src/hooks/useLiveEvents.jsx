/* eslint-disable react-refresh/only-export-components */
import { useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { LiveEventsContext } from './liveEventsContext'

// One EventSource per tab is opened at app mount and multiplexed across
// every consumer through subscribe(types, handler).
const EVENTS_URL = '/api/v1/events'
const EVENT_NAMES = ['findings', 'actions', 'health', 'heartbeat']
const BASE_DELAY_MS = 3000
const MAX_DELAY_MS = 60000
const JITTER = 0.2
const OFFLINE_AFTER = 3

// reconnectDelay is 3 s for the first reconnect we open, doubling up to
// 60 s, with +/-20% jitter so tabs do not all reconnect at once after a
// sidecar restart.
function reconnectDelay(attempt) {
  const base = Math.min(BASE_DELAY_MS * 2 ** (attempt - 1), MAX_DELAY_MS)
  return Math.round(base * (1 + JITTER * (2 * Math.random() - 1)))
}

// statusAfterError: three failures in a row without an open are offline.
// A single browser retry is the routine end of a 45 s server stream, so it
// keeps the current status; a closed source or a second retry is not live.
function statusAfterError(failures, closed, prev) {
  if (failures >= OFFLINE_AFTER) return 'offline'
  if (closed || failures > 1) return 'reconnecting'
  return prev
}

// useEventStream owns the EventSource. The browser retries a dropped
// stream itself (readyState CONNECTING), but a reconnect answered with a
// non-200, such as a proxy 502 during a restart, closes the source for
// good. Then a new source is opened after reconnectDelay. Events update no
// React state, so heartbeats re-render nothing.
function useEventStream(listenersRef) {
  const [status, setStatus] = useState('reconnecting')
  useEffect(() => {
    let es = null
    let timer = null
    let failures = 0 // errors since the last open, browser retries included
    let reconnects = 0 // sources we reopened since the last open
    const dispatch = (evt) => {
      let parsed
      try { parsed = JSON.parse(evt.data) } catch { return }
      listenersRef.current.forEach(fn => {
        try { fn(parsed) } catch { /* listener errors shouldn't kill the stream */ }
      })
    }
    const onError = (source) => {
      if (source !== es || timer !== null) return
      failures += 1
      const count = failures
      const closed = source.readyState === EventSource.CLOSED
      setStatus(prev => statusAfterError(count, closed, prev))
      if (!closed) return
      source.close()
      reconnects += 1
      timer = setTimeout(connect, reconnectDelay(reconnects))
    }
    function connect() {
      timer = null
      // EventSource sends no custom headers; withCredentials sends the cookie.
      const source = new EventSource(EVENTS_URL, { withCredentials: true })
      es = source
      source.onopen = () => {
        if (source !== es) return
        failures = 0
        reconnects = 0
        setStatus('live')
      }
      source.onerror = () => onError(source)
      source.onmessage = dispatch
      EVENT_NAMES.forEach(name => source.addEventListener(name, dispatch))
    }
    connect()
    return () => {
      clearTimeout(timer)
      es.close()
    }
  }, [listenersRef])
  return status
}

export function LiveEventsProvider({ children }) {
  const listenersRef = useRef(new Set())
  const status = useEventStream(listenersRef)

  const subscribe = useCallback((types, handler) => {
    const wanted = Array.isArray(types)
      ? new Set(types)
      : null
    const wrapped = (evt) => {
      if (wanted && !wanted.has(evt.type)) return
      handler(evt)
    }
    listenersRef.current.add(wrapped)
    return () => listenersRef.current.delete(wrapped)
  }, [])

  const value = useMemo(() => ({ subscribe, status }), [subscribe, status])
  return (
    <LiveEventsContext.Provider value={value}>
      {children}
    </LiveEventsContext.Provider>
  )
}

// Outside a provider (leaf-component tests) there is no stream: status is
// null and subscribe is a no-op.
const NO_PROVIDER = { status: null, subscribe: () => () => {} }

// useLiveEvents returns { subscribe, status }; status is 'live',
// 'reconnecting' (not live yet or any more; pages keep polling) or
// 'offline' (three failures in a row).
export function useLiveEvents() {
  return useContext(LiveEventsContext) ?? NO_PROVIDER
}

// LIVE_DEBOUNCE_MS coalesces a burst of events into one refetch.
export const LIVE_DEBOUNCE_MS = 2000

// inScope: with one database selected, an event that names another
// database does not concern the page; an event naming none concerns all.
function inScope(evt, database) {
  if (!database || database === 'all' || !evt.database) return true
  return evt.database === database
}

// useLiveRefetch calls `refetch` when the server pushes an event of any of
// the supplied types, layered on top of useAPI's polling. It is bounded
// (dashboard-08): with `database` selected, other databases' events are
// ignored; a burst refetches once, LIVE_DEBOUNCE_MS after its last event;
// and a hidden tab only notes that the data is dirty and refetches once
// when shown. Pass no database for fleet-wide data.
export function useLiveRefetch(types, refetch, { database } = {}) {
  const { subscribe } = useLiveEvents()
  const refetchRef = useRef(refetch)
  const typesRef = useRef(types)
  useEffect(() => {
    refetchRef.current = refetch
    typesRef.current = types
  })
  const enabled = Boolean(refetch)
  const typesKey = JSON.stringify(types)
  useEffect(() => {
    if (!enabled) return undefined
    let timer = null
    let dirty = false
    const run = () => {
      timer = null
      if (document.hidden) dirty = true
      else refetchRef.current?.()
    }
    const later = () => {
      clearTimeout(timer)
      timer = setTimeout(run, LIVE_DEBOUNCE_MS)
    }
    const onVisibility = () => {
      if (document.hidden || !dirty) return
      dirty = false
      later()
    }
    const unsubscribe = subscribe(typesRef.current, evt => {
      if (!inScope(evt, database)) return
      if (document.hidden) dirty = true
      else later()
    })
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      unsubscribe()
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisibility)
    }
    // typesKey stands for types, so an inline array literal does not resubscribe.
  }, [subscribe, enabled, database, typesKey])
}
