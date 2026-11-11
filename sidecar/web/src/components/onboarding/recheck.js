import { useEffect } from 'react'

// Shared by the first look re-check button and the panels it refreshes
// (firstlook-05).

export const RECHECKED_EVENT = 'pg-sage:first-look-rechecked'

export function canRecheck(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

// useRecheckedRefetch calls refetch after any panel's re-check succeeded.
export function useRecheckedRefetch(refetch) {
  useEffect(() => {
    if (!refetch) return undefined
    const onRechecked = () => refetch()
    window.addEventListener(RECHECKED_EVENT, onRechecked)
    return () => window.removeEventListener(RECHECKED_EVENT, onRechecked)
  }, [refetch])
}
