import { useEffect, useState } from 'react'

// leaderText is "leader", "follower of <holder>" or "follower", or null
// when leader election is off or the status is unreadable.
function leaderText(status) {
  if (!status || typeof status !== 'object' || status.enabled !== true) return null
  if (status.leader === true) return 'leader'
  return typeof status.holder === 'string' && status.holder
    ? `follower of ${status.holder}` : 'follower'
}

// LeaderBadge reads the fleet-learning leader election once (fleet-06).
// It is an aside: a failed read shows nothing rather than an alert.
export function LeaderBadge() {
  const [status, setStatus] = useState(null)
  useEffect(() => {
    let cancelled = false
    async function load() {
      // Every failure of this optional read (the request rejecting, a
      // non-2xx answer, a body that is not JSON) leaves the badge hidden.
      const res = await fetch('/api/v1/fleet/leader', { credentials: 'include' })
        .catch(() => null)
      if (!res?.ok) return
      const body = await res.json().catch(() => null)
      if (!cancelled) setStatus(body)
    }
    load()
    return () => { cancelled = true }
  }, [])
  const text = leaderText(status)
  if (!text) return null
  return (
    <span data-testid="fleet-leader-badge" className="px-2 py-0.5 rounded text-xs font-normal"
      title={status.last_error ? `last election error: ${status.last_error}` : undefined}
      style={{ color: 'var(--text-secondary)', border: '1px solid var(--border)' }}>
      {text}
    </span>
  )
}
