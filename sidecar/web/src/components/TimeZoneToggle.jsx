import { useState } from 'react'
import { Globe } from 'lucide-react'
import { formatTime, setTimeZoneMode, useTimeZoneMode } from '../lib/time'

// TimeZoneToggle is the header choice between local time and UTC for every
// time the dashboard prints (ux-05d). The choice is remembered per viewer.
export function TimeZoneToggle() {
  const mode = useTimeZoneMode()
  // The local zone's name now (CST or CDT), read once per mount.
  const [localZone] = useState(() => formatTime(Date.now(), { zone: 'local' }).split(' ').pop())
  return (
    <div
      className="flex items-center gap-1 px-2 py-1 rounded text-xs"
      data-testid="time-zone-toggle"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
        color: 'var(--text-secondary)',
      }}>
      <Globe size={13} aria-hidden="true" />
      <select
        aria-label="Time zone"
        value={mode}
        onChange={e => setTimeZoneMode(e.target.value)}
        className="bg-transparent text-xs"
        style={{ color: 'var(--text-primary)', border: 'none' }}>
        <option value="local">Local ({localZone})</option>
        <option value="utc">UTC</option>
      </select>
    </div>
  )
}
