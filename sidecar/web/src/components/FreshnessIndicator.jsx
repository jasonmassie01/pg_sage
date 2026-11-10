import { useEffect, useState } from 'react'
import { clockTime as clock } from '../lib/clock'

const TICK_MS = 5000
const LABELS = { live: 'Live', reconnecting: 'Polling', offline: 'Offline' }
const COLORS = {
  live: 'var(--green, #38a169)',
  reconnecting: 'var(--yellow, #d69e2e)',
  offline: 'var(--red, #e53e3e)',
}

function toMillis(value) {
  if (!value) return null
  const ms = new Date(value).getTime()
  return Number.isFinite(ms) && ms > 0 ? ms : null
}

function age(ms, now) {
  const sec = Math.max(0, Math.floor((now - ms) / 1000))
  if (sec < 60) return `${sec} s ago`
  if (sec < 3600) return `${Math.floor(sec / 60)} min ago`
  return `at ${clock(ms)}`
}

// PollingAge ticks while mounted, so "updated 40 s ago" keeps aging
// without a parent re-render. It is aria-hidden: the live region announces
// a status change, not every tick.
function PollingAge({ ms }) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), TICK_MS)
    return () => clearInterval(id)
  }, [])
  return <span aria-hidden="true">, updated {age(ms, now)}</span>
}

function detail(status, ms) {
  if (status === 'live') return ''
  if (ms === null) return ', no data yet'
  return status === 'offline' ? `, data from ${clock(ms)}` : ''
}

// FreshnessIndicator tells the operator whether the page is live (pushed
// events), polling (and how old the data is) or offline (and when its data
// is from). status comes from the live-events provider; updatedAt is the
// time of the last successful fleet poll. It renders nothing without a
// provider (status null).
export function FreshnessIndicator({ status, updatedAt }) {
  if (!LABELS[status]) return null
  const ms = toMillis(updatedAt)
  return (
    <div role="status" data-testid="freshness-indicator" data-status={status}
      title={ms === null ? 'No data loaded yet' : `Last successful refresh at ${clock(ms)}`}
      className="flex items-center gap-1.5 text-xs whitespace-nowrap"
      style={{ color: 'var(--text-secondary)' }}>
      <span aria-hidden="true" className="inline-block rounded-full"
        style={{ width: 8, height: 8, background: COLORS[status] }} />
      <span>{LABELS[status]}{detail(status, ms)}</span>
      {status === 'reconnecting' && ms !== null && <PollingAge key={ms} ms={ms} />}
    </div>
  )
}
