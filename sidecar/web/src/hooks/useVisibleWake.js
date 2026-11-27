import { useEffect, useState } from 'react'

// useVisibleWake returns a counter that grows each time the tab becomes
// visible. Hooks that skip work while document.hidden list it as an effect
// dependency to catch up once on show. It is React state, so every hook
// woken by one visibilitychange (and a time-range tick from the same event)
// renders in one batch.
export function useVisibleWake() {
  const [wake, setWake] = useState(0)
  useEffect(() => {
    const onChange = () => {
      if (!document.hidden) setWake(w => w + 1)
    }
    document.addEventListener('visibilitychange', onChange)
    return () => document.removeEventListener('visibilitychange', onChange)
  }, [])
  return wake
}
