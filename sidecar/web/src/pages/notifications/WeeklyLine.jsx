import { useEffect, useState } from 'react'
import { WEEKLY_LINE_URL } from './quietMute'

// ux-09b: "N messages, M led to an action" over the last 7 days.
export function WeeklyLine() {
  const [line, setLine] = useState(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let live = true
    fetch(WEEKLY_LINE_URL, { credentials: 'include' })
      .then(res => (res.ok ? res.json() : Promise.reject(new Error(String(res.status)))))
      .then(data => { if (live) setLine(data) })
      .catch(() => { if (live) setFailed(true) })
    return () => { live = false }
  }, [])

  if (failed) {
    return (
      <p data-testid="weekly-line" className="text-sm mb-4"
        style={{ color: 'var(--text-secondary)' }}>
        This week: not available
      </p>
    )
  }
  if (!line || typeof line.text !== 'string') return null
  return (
    <p data-testid="weekly-line" className="text-sm mb-4"
      style={{ color: 'var(--text-primary)' }}
      title="Sent messages of the last 7 days, and those followed within 24 hours by an
 approve, reject, snooze or mute">
      This week: {line.text}
    </p>
  )
}
