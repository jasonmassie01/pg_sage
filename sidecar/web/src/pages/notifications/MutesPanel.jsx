import { useCallback, useEffect, useState } from 'react'
import { cardStyle, StatusMessages } from './shared'
import { MuteDialog } from './MuteDialog'
import { apiError, classLabel, MUTES_URL } from './quietMute'

// ux-09b: the database mutes in force, Mute database and Unmute. A link
// (#/notifications?mute=<db>&class=<class>, from a message footer or a
// database row) opens the dialog filled in.
export function MutesPanel({ link }) {
  const [mutes, setMutes] = useState([])
  const [error, setError] = useState(null)
  const [success, setSuccess] = useState(null)
  const [dialog, setDialog] = useState(link?.database ? link : null)

  const load = useCallback(async () => {
    try {
      const res = await fetch(MUTES_URL, { credentials: 'include' })
      if (!res.ok) throw new Error(await apiError(res, 'Failed to load mutes'))
      setMutes((await res.json()).mutes || [])
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => { load() }, [load])

  async function unmute(m) {
    setError(null)
    setSuccess(null)
    const q = new URLSearchParams({ database: m.database })
    if (m.class) q.set('class', m.class)
    try {
      const res = await fetch(`${MUTES_URL}?${q}`, { method: 'DELETE', credentials: 'include' })
      if (!res.ok) throw new Error(await apiError(res, 'Unmute failed'))
      setSuccess(`Unmuted ${m.database}`)
      load()
    } catch (err) {
      setError(err.message)
    }
  }

  function saved(m) {
    setDialog(null)
    setSuccess(`Muted ${m.database} (${classLabel(m.class)}) until ` +
      new Date(m.until).toLocaleString())
    load()
  }

  return (
    <div className="rounded-lg p-4 mb-4" style={cardStyle} data-testid="mutes-panel">
      <div className="flex items-center justify-between mb-2">
        <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
          Mutes
        </h2>
        <button data-testid="mute-database-button" onClick={() => setDialog({})}
          className="px-3 py-1 rounded text-xs" style={{ color: 'var(--accent)' }}>
          Mute database
        </button>
      </div>
      <StatusMessages error={error} success={success} />
      <MuteList mutes={mutes} onUnmute={unmute} />
      {dialog && <MuteDialog initial={dialog} onSaved={saved}
        onCancel={() => setDialog(null)} />}
    </div>
  )
}

function MuteList({ mutes, onUnmute }) {
  if (!mutes.length) {
    return (
      <p className="text-xs" data-testid="mutes-empty" style={{ color: 'var(--text-secondary)' }}>
        No database is muted.
      </p>
    )
  }
  return (
    <ul className="text-sm">
      {mutes.map(m => (
        <li key={`${m.database}/${m.class}`} data-testid="mute-row"
          className="flex items-center justify-between py-1"
          style={{ borderBottom: '1px solid var(--border)' }}>
          <span style={{ color: 'var(--text-primary)' }}>
            {m.database} - {classLabel(m.class)} - until {new Date(m.until).toLocaleString()}
            <span className="text-xs ml-2" style={{ color: 'var(--text-secondary)' }}>
              by {m.actor}{m.reason ? `: ${m.reason}` : ''}
            </span>
          </span>
          <button data-testid="unmute-button" onClick={() => onUnmute(m)}
            className="px-2 py-1 rounded text-xs" style={{ color: 'var(--accent)' }}>
            Unmute
          </button>
        </li>
      ))}
    </ul>
  )
}
