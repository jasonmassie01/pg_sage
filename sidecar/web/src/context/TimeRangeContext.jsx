/* eslint-disable react-refresh/only-export-components */
import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import { readPref, writePref } from '../lib/storage'

const RANGES = [
  { key: '1h',  label: 'Last 1h',  ms:         60 * 60 * 1000 },
  { key: '6h',  label: 'Last 6h',  ms:     6 * 60 * 60 * 1000 },
  { key: '24h', label: 'Last 24h', ms:    24 * 60 * 60 * 1000 },
  { key: '7d',  label: 'Last 7d',  ms: 7 * 24 * 60 * 60 * 1000 },
  { key: '30d', label: 'Last 30d', ms:    30*24*60*60*1000     },
]

const TimeRangeContext = createContext(null)
const DEFAULT_RANGE = RANGES[2]
const MINUTE_MS = 60 * 1000

// rangeFor returns the range a key names; an unknown key (a stale or
// hand-edited pg_sage_range) reads as the 24h default.
function rangeFor(key) {
  return RANGES.find(r => r.key === key) || DEFAULT_RANGE
}

// windowFor is the range's window as a fetch at time `ms` asks for it
// (dashboard-07): it ends at the next minute boundary, so the newest row is
// never cut off and requests within one minute ask for the same window.
function windowFor(range, ms) {
  const to = new Date(Math.ceil(ms / MINUTE_MS) * MINUTE_MS)
  const from = new Date(to.getTime() - range.ms)
  return { from, to, fromISO: from.toISOString(), toISO: to.toISOString() }
}

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

// TimeRangeProvider holds the picker's range. from/to move with the 30 s
// tick (the Actions ledger uses them); windowAt(ms) is stable until the
// range changes, for a poll that computes its window when it fetches.
export function TimeRangeProvider({ children }) {
  const [rangeKey, setRangeKey] = useState(() => rangeFor(readPref('pg_sage_range', '24h')).key)
  const nowTick = useNowTick()
  const range = rangeFor(rangeKey)
  const windowAt = useCallback(ms => windowFor(range, ms), [range])

  useEffect(() => {
    writePref('pg_sage_range', rangeKey)
  }, [rangeKey])

  const value = useMemo(() => {
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
      windowAt,
    }
  }, [rangeKey, range, nowTick, windowAt])

  return (
    <TimeRangeContext.Provider value={value}>
      {children}
    </TimeRangeContext.Provider>
  )
}

const defaultWindowAt = ms => windowFor(DEFAULT_RANGE, ms)

export function useTimeRange() {
  const ctx = useContext(TimeRangeContext)
  if (!ctx) {
    // Safe default when the provider is absent (e.g. login screen).
    const to = new Date()
    const from = new Date(to.getTime() - 24 * 60 * 60 * 1000)
    return {
      rangeKey: '24h',
      setRangeKey: () => {},
      range: DEFAULT_RANGE,
      ranges: RANGES,
      from, to,
      fromISO: from.toISOString(),
      toISO: to.toISOString(),
      windowAt: defaultWindowAt,
    }
  }
  return ctx
}
