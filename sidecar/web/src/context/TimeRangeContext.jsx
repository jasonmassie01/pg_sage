/* eslint-disable react-refresh/only-export-components */
import { createContext, useContext, useEffect, useMemo, useState } from 'react'

const RANGES = [
  { key: '1h',  label: 'Last 1h',  ms:         60 * 60 * 1000 },
  { key: '6h',  label: 'Last 6h',  ms:     6 * 60 * 60 * 1000 },
  { key: '24h', label: 'Last 24h', ms:    24 * 60 * 60 * 1000 },
  { key: '7d',  label: 'Last 7d',  ms: 7 * 24 * 60 * 60 * 1000 },
  { key: '30d', label: 'Last 30d', ms:    30*24*60*60*1000     },
]

const TimeRangeContext = createContext(null)

export function TimeRangeProvider({ children }) {
  const [rangeKey, setRangeKey] = useState(() => {
    return localStorage.getItem('pg_sage_range') || '24h'
  })
  const [nowTick, setNowTick] = useState(() => Date.now())

  useEffect(() => {
    localStorage.setItem('pg_sage_range', rangeKey)
  }, [rangeKey])

  useEffect(() => {
    const id = setInterval(() => setNowTick(Date.now()), 30000)
    return () => clearInterval(id)
  }, [])

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
