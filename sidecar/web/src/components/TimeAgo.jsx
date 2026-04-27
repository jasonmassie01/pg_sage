import { useEffect, useState } from 'react'

function formatAgo(sec) {
  if (sec < 60) return `${sec}s ago`
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`
  return `${Math.floor(sec / 86400)}d ago`
}

// TimeAgo is a static string rendered on mount. Use LiveTimeAgo for
// values that should tick without a parent re-render.
export function TimeAgo({ timestamp }) {
  if (!timestamp) {
    return <span style={{ color: 'var(--text-secondary)' }}>--</span>
  }
  const sec = Math.floor(
    (Date.now() - new Date(timestamp).getTime()) / 1000,
  )
  return (
    <span style={{ color: 'var(--text-secondary)' }}>
      {formatAgo(sec)}
    </span>
  )
}

// LiveTimeAgo ticks every 15s so "last seen" labels stay honest even
// when the surrounding page hasn't refetched. Returns '--' for empty
// timestamps.
export function LiveTimeAgo({ timestamp, tickMs = 15000 }) {
  const [, setNow] = useState(Date.now())
  useEffect(() => {
    if (!timestamp) return
    const id = setInterval(() => setNow(Date.now()), tickMs)
    return () => clearInterval(id)
  }, [timestamp, tickMs])
  if (!timestamp) {
    return <span style={{ color: 'var(--text-secondary)' }}>--</span>
  }
  const sec = Math.floor(
    (Date.now() - new Date(timestamp).getTime()) / 1000,
  )
  return (
    <span
      data-testid="live-time-ago"
      style={{ color: 'var(--text-secondary)' }}>
      {formatAgo(sec)}
    </span>
  )
}
