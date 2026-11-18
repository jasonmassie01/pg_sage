import { useState } from 'react'
import { FormField, inputStyle, StatusMessages } from './shared'
import { apiError, quietHoursURL } from './quietMute'

// ux-09b: a channel's own quiet hours on the channel form. Inside them
// its Ask and Digest messages wait until the window ends; a Page goes out
// at once. Empty fields keep the global alerting quiet hours.
export function QuietHoursForm({ channel, onSaved }) {
  const [start, setStart] = useState(channel.quiet_hours_start || '')
  const [end, setEnd] = useState(channel.quiet_hours_end || '')
  const [tz, setTz] = useState(channel.quiet_hours_timezone || '')
  const [error, setError] = useState(null)
  const [success, setSuccess] = useState(null)

  async function save(body) {
    setError(null)
    setSuccess(null)
    try {
      const res = await fetch(quietHoursURL(channel.id), {
        method: 'PUT', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) throw new Error(await apiError(res, 'Quiet hours not saved'))
      setSuccess(body.start ? 'Quiet hours saved' : 'Quiet hours cleared')
      if (onSaved) onSaved()
    } catch (err) {
      setError(err.message)
    }
  }

  function clear() {
    setStart('')
    setEnd('')
    setTz('')
    save({ start: '', end: '', timezone: '' })
  }

  return (
    <div className="mt-4 pt-4" data-testid="quiet-hours-form"
      style={{ borderTop: '1px solid var(--border)' }}>
      <h3 className="text-xs font-semibold mb-2" style={{ color: 'var(--text-primary)' }}>
        Quiet hours
      </h3>
      <p className="text-xs mb-2" style={{ color: 'var(--text-secondary)' }}>
        Ask and Digest messages wait until the window ends. Pages, the emergency stop,
        a failed rollback and a freeze red line always go out at once.
      </p>
      <StatusMessages error={error} success={success} />
      <div className="flex flex-wrap gap-3 items-end">
        <FormField label="From (HH:MM)">
          <input data-testid="quiet-start" value={start} placeholder="22:00"
            onChange={e => setStart(e.target.value)}
            className="px-3 py-1.5 rounded text-sm w-24" style={inputStyle} />
        </FormField>
        <FormField label="To (HH:MM)">
          <input data-testid="quiet-end" value={end} placeholder="07:00"
            onChange={e => setEnd(e.target.value)}
            className="px-3 py-1.5 rounded text-sm w-24" style={inputStyle} />
        </FormField>
        <FormField label="Timezone">
          <input data-testid="quiet-timezone" value={tz} placeholder="America/Chicago"
            onChange={e => setTz(e.target.value)}
            className="px-3 py-1.5 rounded text-sm w-48" style={inputStyle} />
        </FormField>
        <button type="button" data-testid="quiet-save"
          onClick={() => save({ start, end, timezone: tz })}
          className="px-3 py-1.5 rounded text-sm"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          Save quiet hours
        </button>
        <button type="button" data-testid="quiet-clear" onClick={clear}
          className="px-3 py-1.5 rounded text-sm" style={{ color: 'var(--text-secondary)' }}>
          Clear
        </button>
      </div>
    </div>
  )
}
