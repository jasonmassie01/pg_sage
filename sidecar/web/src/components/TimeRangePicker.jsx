import { Clock } from 'lucide-react'
import { useTimeRange } from '../context/TimeRangeContext'

export function TimeRangePicker() {
  const { rangeKey, setRangeKey, ranges } = useTimeRange()
  return (
    <div
      className="flex items-center gap-1 px-2 py-1 rounded text-xs"
      data-testid="time-range-picker"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
        color: 'var(--text-secondary)',
      }}>
      <Clock size={13} aria-hidden="true" />
      <select
        aria-label="Time range"
        value={rangeKey}
        onChange={e => setRangeKey(e.target.value)}
        className="bg-transparent text-xs"
        style={{ color: 'var(--text-primary)', border: 'none' }}>
        {ranges.map(r => (
          <option key={r.key} value={r.key}>
            {r.label}
          </option>
        ))}
      </select>
    </div>
  )
}
