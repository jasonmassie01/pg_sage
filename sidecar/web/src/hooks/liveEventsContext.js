import { createContext, useContext } from 'react'

// LiveEventsContext carries { subscribe, status } from LiveEventsProvider.
// It lives apart from the provider so a header that only reads the status
// does not depend on the provider module.
export const LiveEventsContext = createContext(null)

// useLiveStatus returns 'live', 'reconnecting' or 'offline' inside a
// LiveEventsProvider, and null outside one (no stream exists to report).
export function useLiveStatus() {
  return useContext(LiveEventsContext)?.status ?? null
}
