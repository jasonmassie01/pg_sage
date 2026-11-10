/* eslint-disable react-refresh/only-export-components */
import { createContext, useContext, useEffect, useMemo, useState } from 'react'
import { readPref, writePref } from '../lib/storage'

const RANGES = [
  { key: '1h',  label: 'Last 1h',  ms:         60 * 60 * 1000 },
  { key: '6h',  label: 'Last 6h',  ms:     6 * 60 * 60 * 1000 },
  { key: '24h', label: 'Last 24h', ms:    24 * 60 * 60 * 1000 },
  { key: '7d',  label: 'Last 7d',  ms: 7 * 24 * 60 * 60 * 1000 },
  { key: '30d', label: 'Last 30d', ms:    30*24*60*60*1000     },
]

const TimeRangeContext = createContext(null)

// TICK_MS is how often the window's end moves to now while the tab is shown.
const TICK_MS = 30000

// useNowTick returns the time the window ends at, moved to now every
// TICK_MS. Every tick moves from/to and so refetches each windowed page, so
// a hidden tab does not tick; showing it ticks once if a tick fell due, and
// the next tick counts from that one.
function useNowTick() {
  const [nowTick, setNowTick] = useState(() => Date.now())
  useEffect(() => {
    let last = Date.now()
    let timer = null
    const tick = () => {
      last = Date.now()
      setNowTick(last)
      clearTimeout(timer)
      timer = setTimeout(onTimer, TICK_MS)
    }
    function onTimer() {
      timer = null
      if (!document.hidden) tick()
    }
    const onVisibility = () => {
      if (!document.hidden && Date.now() - last >= TICK_MS) tick()
    }
    timer = setTimeout(onTimer, TICK_MS)
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [])
  return nowTick
}

export function TimeRangeProvider({ children }) {
  const [rangeKey, setRangeKey] = useState(() => readPref('pg_sage_range', '24h'))
  const nowTick = useNowTick()

  useEffect(() => {
    writePref('pg_sage_range', rangeKey)
  }, [rangeKey])

  const value = useMemo(() => {
    const range = RANGES.find(r => r.key === rangeKey) || RANGES[2]
    const to = new Date(nowTick)
    const from = new Date(nowTick - range.ms)
    return {
      rangeKey,
      setRangeKey,
      range,
      ranges: RANGES,
      from,
      to,
      fromISO: from.toISOString(),
      toISO: to.toISOString(),
    }
  }, [rangeKey, nowTick])

  return (
    <TimeRangeContext.Provider value={value}>
      {children}
    </TimeRangeContext.Provider>
  )
}

export function useTimeRange() {
  const ctx = useContext(TimeRangeContext)
  if (!ctx) {
    // Safe default when the provider is absent (e.g. login screen).
    const to = new Date()
    const from = new Date(to.getTime() - 24 * 60 * 60 * 1000)
    return {
      rangeKey: '24h',
      setRangeKey: () => {},
      range: RANGES[2],
      ranges: RANGES,
      from, to,
      fromISO: from.toISOString(),
      toISO: to.toISOString(),
    }
  }
  return ctx
}
