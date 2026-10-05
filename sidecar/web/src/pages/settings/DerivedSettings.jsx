import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { ErrorBanner } from '../../components/ErrorBanner'

// Derived settings (roadmap phase 3, self-configuration): per database,
// every key pg_sage derives from evidence, with its value, status
// (default, derived, shadow, pinned, operator), pending restart, cited
// evidence, bounds and history. Admins can pin the current value or unpin.

const muted = { color: 'var(--text-secondary)' }

function derivedURL(database) {
  return database && database !== 'all'
    ? `/api/v1/derived-settings?database=${encodeURIComponent(database)}`
    : '/api/v1/derived-settings'
}

function fmt(value, unit) {
  if (value === null || value === undefined) return '-'
  return unit ? `${value} ${unit}` : `${value}`
}

const statusColor = {
  derived: 'var(--green)', shadow: 'var(--yellow)', pinned: 'var(--accent)',
  operator: 'var(--text-primary)', default: 'var(--text-secondary)',
}

function StatusBadge({ status }) {
  return (
    <span className="text-xs px-2 py-0.5 rounded"
      style={{ border: '1px solid var(--border)', color: statusColor[status] }}>
      {status}
    </span>
  )
}

function Evidence({ evidence }) {
  if (!evidence || evidence.length === 0) return null
  return (
    <div className="text-xs" style={muted}>
      Evidence:{' '}
      {evidence.map(e => `${e.name} ${e.value}${e.unit ? ` ${e.unit}` : ''}`).join(', ')}
    </div>
  )
}

function History({ history, unit }) {
  if (!history || history.length === 0) {
    return <div className="text-xs" style={muted}>No ledger entries yet.</div>
  }
  return (
    <ul className="text-xs space-y-1" style={muted}>
      {history.map((h, i) => (
        <li key={`${h.at}-${i}`}>
          <span style={{ color: 'var(--text-primary)' }}>{h.event}</span>{' '}
          {h.previous !== null && h.previous !== undefined
            ? `${h.previous} → ${h.value}` : fmt(h.value, unit)}{' '}
          · {h.actor} · {h.at} · <span>{h.reason}</span>
        </li>
      ))}
    </ul>
  )
}

function stateLine(s) {
  const parts = []
  if (s.status === 'operator') parts.push('set in configuration; never derived')
  if (s.shadow) {
    parts.push(`shadow ${fmt(s.shadow.value, s.unit)} since ${s.shadow.since}` +
      ` (${s.shadow.samples} samples, soak ${s.shadow.soak_hours} h)` +
      (s.shadow.reason ? `: ${s.shadow.reason}` : ''))
  }
  if (s.pending_restart !== null && s.pending_restart !== undefined) {
    parts.push(`${fmt(s.pending_restart, s.unit)} pending restart`)
  }
  if (s.pinned) parts.push(`pinned by ${s.pinned.by} at ${s.pinned.at}`)
  if (s.note) parts.push(s.note)
  return parts
}

function SettingRow({ setting: s, onPin, busy }) {
  const [open, setOpen] = useState(false)
  const canPin = s.status !== 'operator' && s.status !== 'pinned'
  return (
    <div data-testid={`derived-${s.key}`} className="py-3 space-y-1"
      style={{ borderTop: '1px solid var(--border)' }}>
      <div className="flex items-center gap-2 flex-wrap">
        <code className="text-sm" style={{ color: 'var(--text-primary)' }}>{s.key}</code>
        <span className="text-sm font-semibold">{fmt(s.value, s.unit)}</span>
        <StatusBadge status={s.status} />
        <span className="text-xs" style={muted}>
          default {fmt(s.default, s.unit)} · bounds{' '}
          {`${s.bounds?.min}–${s.bounds?.max} ${s.unit}`} · {s.lifecycle}
        </span>
        <span className="flex-1" />
        {canPin && (
          <button data-testid={`pin-${s.key}`} disabled={busy}
            onClick={() => onPin(s.key, 'pin')} className="text-xs px-2 py-1 rounded"
            style={{ border: '1px solid var(--border)' }}>Pin current</button>
        )}
        {s.status === 'pinned' && (
          <button data-testid={`unpin-${s.key}`} disabled={busy}
            onClick={() => onPin(s.key, 'unpin')} className="text-xs px-2 py-1 rounded"
            style={{ border: '1px solid var(--border)' }}>Unpin</button>
        )}
        <button data-testid={`history-${s.key}`} onClick={() => setOpen(!open)}
          className="text-xs px-2 py-1 rounded" style={muted}>
          {open ? 'Hide history' : 'History'}
        </button>
      </div>
      <div className="text-xs" style={muted}>{s.summary}</div>
      {stateLine(s).map(line => (
        <div key={line} className="text-xs" style={muted}>{line}</div>
      ))}
      <Evidence evidence={s.evidence} />
      {open && <History history={s.history} unit={s.unit} />}
    </div>
  )
}

async function changePin(database, key, action) {
  const res = await fetch(
    `/api/v1/derived-settings/${encodeURIComponent(key)}/${action}` +
    `?database=${encodeURIComponent(database)}`,
    {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' }, body: '{}',
    })
  if (res.ok) return null
  let body = {}
  try { body = await res.json() } catch { /* non-JSON error body */ }
  return body.error || `${action} failed (${res.status})`
}

export function DerivedSettings({ database }) {
  const url = derivedURL(database)
  const { data, loading, error, refetch } = useAPI(url, 60000)
  const [failure, setFailure] = useState(null)
  const [busy, setBusy] = useState(false)

  if (loading && !data) return null
  if (error) return <ErrorBanner message={`Derived settings: ${error}`} onRetry={refetch} />

  async function onPin(db, key, action) {
    setBusy(true)
    setFailure(null)
    try {
      const msg = await changePin(db, key, action)
      if (msg) setFailure(msg)
      else refetch()
    } catch (err) {
      setFailure(err.message)
    } finally {
      setBusy(false)
    }
  }

  const databases = Array.isArray(data?.databases) ? data.databases : []
  return (
    <section data-testid="derived-settings" data-url={url} className="rounded p-4 space-y-2"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
        Derived settings
      </h2>
      <p className="text-xs" style={muted}>{data?.meaning}</p>
      {failure && <ErrorBanner message={failure} />}
      {databases.length === 0 && (
        <div className="text-sm" style={muted}>No derived settings for this selection.</div>
      )}
      {databases.map(db => (
        <div key={db.database}>
          {databases.length > 1 && (
            <h3 className="text-xs font-semibold mt-2">{db.database}</h3>
          )}
          {db.settings.map(s => (
            <SettingRow key={s.key} setting={s} busy={busy}
              onPin={(key, action) => onPin(db.database, key, action)} />
          ))}
        </div>
      ))}
    </section>
  )
}
