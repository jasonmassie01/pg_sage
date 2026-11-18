/* eslint-disable react-refresh/only-export-components */
import { useCallback } from 'react'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip,
  ResponsiveContainer, Legend,
} from 'recharts'
import { useAPI, withTimeRange } from '../hooks/useAPI'
import { useTimeRange } from '../context/TimeRangeContext'
import { formatClock, formatTime } from '../lib/time'

// Stable colour ramp per-database. Recharts picks default colours
// that drift when series count changes, which makes the Overview
// chart jumpy between refreshes. We hash the name to an index
// into a short palette so colours stay the same across renders.
const PALETTE = [
  '#4c8bf5', '#5ac882', '#f0a54a', '#a07cf2',
  '#ff8a8a', '#9dc7ff', '#ffd666', '#6ad4c1',
]
function colorFor(name) {
  let h = 0
  for (let i = 0; i < name.length; i++) {
    h = (h * 31 + name.charCodeAt(i)) | 0
  }
  return PALETTE[Math.abs(h) % PALETTE.length]
}

// mergeSeries takes the per-database point arrays returned by
// /api/v1/fleet/health and merges them into a single array of
// rows keyed by timestamp: [{ t, series_0: 87, series_1: 91 }, ...].
// Recharts treats dotted dataKey strings as object paths, so database
// aliases must be display names only, not object keys.
export function mergeSeries(perDB) {
  const byTs = new Map()
  const series = Object.keys(perDB).sort().map((name, i) => ({
    name,
    key: `series_${i}`,
  }))
  for (const item of series) {
    const name = item.name
    for (const p of perDB[name] || []) {
      const k = new Date(p.t).getTime()
      if (!byTs.has(k)) byTs.set(k, { t: k })
      byTs.get(k)[item.key] = p.health
    }
  }
  return {
    rows: Array.from(byTs.values()).sort((a, b) => a.t - b.t),
    series,
  }
}

// formatTick is an axis label in the header's local or UTC choice, with
// its zone, matching the tooltip's formatTime (ux-05d review).
export function formatTick(ms) {
  return formatClock(ms)
}

// FleetHealthChart polls the picker's window every 60 s. The window is
// computed when each request starts (dashboard-07), so the time range's
// 30 s tick does not refetch the chart between polls.
export function FleetHealthChart({ database }) {
  const { windowAt } = useTimeRange()
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const base = `/api/v1/fleet/health${dbParam}`
  const url = useCallback(() => withTimeRange(base, windowAt(Date.now())), [base, windowAt])
  const { data, loading, error } = useAPI(url, 60000)

  if (loading && !data) {
    return (
      <div
        data-testid="fleet-health-chart-skeleton"
        className="h-48 rounded animate-pulse"
        style={{ background: 'var(--bg-hover)' }}
      />
    )
  }
  if (error) {
    return (
      <div className="text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        Health history unavailable: {error}
      </div>
    )
  }
  const perDB = data?.databases || {}
  const { rows, series } = mergeSeries(perDB)
  if (rows.length === 0) {
    return (
      <div className="text-xs"
        data-testid="fleet-health-chart-empty"
        style={{ color: 'var(--text-secondary)' }}>
        No health samples recorded yet for this window.
      </div>
    )
  }

  return (
    <div
      data-testid="fleet-health-chart"
      className="rounded p-4"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div
        className="flex items-center justify-between mb-2">
        <h2 className="text-sm font-medium"
          style={{ color: 'var(--text-secondary)' }}>
          Fleet Health Over Time
        </h2>
      </div>
      <div style={{ width: '100%', height: 220 }}>
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={rows}>
            <CartesianGrid strokeDasharray="3 3"
              stroke="var(--border)" />
            <XAxis
              dataKey="t"
              tickFormatter={formatTick}
              tick={{ fill: 'var(--text-secondary)', fontSize: 11 }}
              stroke="var(--border)"
            />
            <YAxis
              domain={[0, 100]}
              tick={{ fill: 'var(--text-secondary)', fontSize: 11 }}
              stroke="var(--border)"
            />
            <Tooltip
              labelFormatter={ms => formatTime(ms)}
              contentStyle={{
                background: 'var(--bg-card)',
                border: '1px solid var(--border)',
                color: 'var(--text-primary)',
              }}
            />
            <Legend wrapperStyle={{ fontSize: 11 }} />
            {series.map(s => (
              <Line key={s.key}
                type="monotone"
                dataKey={s.key}
                name={s.name}
                stroke={colorFor(s.name)}
                strokeWidth={2}
                dot={false}
                isAnimationActive={false}
              />
            ))}
          </LineChart>
        </ResponsiveContainer>
      </div>
    </div>
  )
}
