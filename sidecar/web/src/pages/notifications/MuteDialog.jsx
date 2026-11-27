import { useState } from 'react'
import { cardStyle, FormField, inputStyle, StatusMessages } from './shared'
import { apiError, MUTE_CLASSES, MUTE_DURATIONS, MUTES_URL, muteUntil } from './quietMute'

// ux-09b: mute one database's messages, of one class or every class, for
// at most 90 days. Pages for the emergency stop, a failed rollback and a
// freeze red line are never muted.
export function MuteDialog({ initial, onSaved, onCancel }) {
  const [database, setDatabase] = useState(initial?.database || '')
  const [klass, setKlass] = useState(initial?.class || '')
  const [hours, setHours] = useState(24)
  const [reason, setReason] = useState('')
  const [error, setError] = useState(null)

  async function submit(e) {
    e.preventDefault()
    setError(null)
    try {
      const res = await fetch(MUTES_URL, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ database: database.trim(), class: klass,
          until: muteUntil(hours), reason: reason.trim() }),
      })
      if (!res.ok) throw new Error(await apiError(res, 'Mute not saved'))
      onSaved(await res.json())
    } catch (err) {
      setError(err.message)
    }
  }

  return (
    <div role="dialog" aria-modal="true" aria-label="Mute database"
      onKeyDown={e => { if (e.key === 'Escape') onCancel() }}
      className="fixed inset-0 z-50 flex items-start justify-center p-4 overflow-y-auto"
      style={{ background: 'rgba(0,0,0,0.5)' }}>
      <form onSubmit={submit} className="rounded-lg p-4 w-full max-w-lg mt-12"
        style={cardStyle}>
        <h2 className="text-sm font-semibold mb-3" style={{ color: 'var(--text-primary)' }}>
          Mute database
        </h2>
        <StatusMessages error={error} />
        <div className="grid gap-3">
          <FormField label="Database">
            <input data-testid="mute-database" value={database} required
              onChange={e => setDatabase(e.target.value)}
              className="px-3 py-1.5 rounded text-sm w-full" style={inputStyle} />
          </FormField>
          <FormField label="Class">
            <select data-testid="mute-class" value={klass}
              onChange={e => setKlass(e.target.value)}
              className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
              {MUTE_CLASSES.map(c => <option key={c.value} value={c.value}>{c.label}</option>)}
            </select>
          </FormField>
          <FormField label="For">
            <select data-testid="mute-duration" value={hours}
              onChange={e => setHours(Number(e.target.value))}
              className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
              {MUTE_DURATIONS.map(d => <option key={d.hours} value={d.hours}>{d.label}</option>)}
            </select>
          </FormField>
          <FormField label="Reason (optional)">
            <input data-testid="mute-reason" value={reason} maxLength={500}
              onChange={e => setReason(e.target.value)}
              className="px-3 py-1.5 rounded text-sm w-full" style={inputStyle} />
          </FormField>
          <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            Pages for the emergency stop, a failed rollback and a freeze red line still go out.
          </p>
        </div>
        <div className="flex gap-2 justify-end mt-4">
          <button type="button" onClick={onCancel} data-testid="mute-cancel"
            className="px-3 py-1.5 rounded text-sm" style={{ color: 'var(--text-secondary)' }}>
            Cancel
          </button>
          <button type="submit" data-testid="mute-submit"
            className="px-3 py-1.5 rounded text-sm"
            style={{ background: 'var(--accent-solid)', color: '#fff' }}>
            Mute
          </button>
        </div>
      </form>
    </div>
  )
}
