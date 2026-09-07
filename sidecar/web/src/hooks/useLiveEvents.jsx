/* eslint-disable react-refresh/only-export-components */
import {
  useEffect,
  useState,
  useRef,
  useCallback,
  createContext,
  useContext,
} from 'react'

// LiveEventsContext exposes a subscribe(types, handler) method that
// pages + hooks use to react to server-pushed change notifications.
// A single EventSource is opened at app-mount and multiplexed
// across all consumers so we're not fanning out N connections.
const LiveEventsContext = createContext(null)

export function LiveEventsProvider({ children }) {
  const listenersRef = useRef(new Set())
  const [connected, setConnected] = useState(false)
  const [lastEventAt, setLastEventAt] = useState(null)

  useEffect(() => {
    // EventSource doesn't support custom headers, but the session
    // cookie is sent automatically thanks to withCredentials.
    const es = new EventSource(
      '/api/v1/events',
      { withCredentials: true }
    )

    const dispatch = (evt) => {
      let parsed
      try { parsed = JSON.parse(evt.data) }
      catch { return }
      setLastEventAt(parsed.ts || new Date().toISOString())
      listenersRef.current.forEach(fn => {
        try { fn(parsed) } catch { /* listener errors shouldn't kill the stream */ }
      })
    }

    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)
    // Catch-all + specific named events (the server emits both).
    es.onmessage = dispatch
    ;['findings', 'actions', 'health', 'heartbeat'].forEach(name => {
      es.addEventListener(name, dispatch)
    })

    return () => {
      es.close()
      setConnected(false)
    }
  }, [])

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

  const value = { subscribe, connected, lastEventAt }
  return (
    <LiveEventsContext.Provider value={value}>
      {children}
    </LiveEventsContext.Provider>
  )
}

// useLiveEvents returns { connected, lastEventAt, subscribe }.
// If used outside a provider, returns a no-op shim so tests that
// render leaf components without wrapping still work.
export function useLiveEvents() {
  const ctx = useContext(LiveEventsContext)
  if (!ctx) {
    return {
      connected: false,
      lastEventAt: null,
      subscribe: () => () => {},
    }
  }
  return ctx
}

// useLiveRefetch calls `refetch` whenever the server pushes an
// event of any of the supplied types. Useful to layer on top of
// useAPI for pages that want live updates without hard-coding the
// polling interval down.
export function useLiveRefetch(types, refetch) {
  const { subscribe } = useLiveEvents()
  useEffect(() => {
    if (!refetch) return undefined
    return subscribe(types, () => refetch())
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [subscribe, refetch, JSON.stringify(types)])
}
