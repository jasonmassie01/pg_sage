import { useCallback, useEffect, useState } from 'react'
import { formatTime } from '../../lib/time'

const LIST_URL = '/api/v1/databases/discovered'
const EXTEND_HOURS = 24

// DiscoveredDatabases is the Discovered tab (market-06): what the fleet
// discovery sources found, with adopt, ignore and extend-TTL.
export function DiscoveredDatabases() {
  const [data, setData] = useState(null)
  const [unavailable, setUnavailable] = useState(false)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(null)

  const load = useCallback(async () => {
    try {
      const res = await fetch(LIST_URL, { credentials: 'include' })
      if (res.status === 404) {
        setUnavailable(true)
        return
      }
      if (!res.ok) throw new Error(`Failed to load discovered databases (HTTP ${res.status})`)
      const body = await res.json()
      setData({ discovered: body.discovered || [], sources: body.sources || [] })
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => { load() }, [load])

  async function act(id, action, body) {
    setBusy(`${action}-${id}`)
    setError(null)
    try {
      const res = await fetch(`${LIST_URL}/${id}/${action}`, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: body ? JSON.stringify(body) : undefined,
      })
      if (!res.ok) {
        const msg = await res.json().catch(() => ({}))
        throw new Error(msg.error || `${action} failed (HTTP ${res.status})`)
      }
      await load()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(null)
    }
  }

  if (unavailable) {
    return (
      <p data-testid="discovery-unavailable" className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        Discovery needs a meta-db fleet with fleet.discovery enabled.
      </p>
    )
  }
  return (
    <div className="space-y-3" data-testid="discovered-databases">
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        Discovered members run at trust observation with approval until an owner adopts
        them. An unadopted member is disabled at its expiry; one its source no longer
        reports is disabled and marked gone.
      </p>
      {error && <ErrorBox message={error} />}
      {data && <SourceErrors sources={data.sources} />}
      {data && data.discovered.length === 0 && (
        <p data-testid="discovered-empty" className="text-sm"
          style={{ color: 'var(--text-secondary)' }}>Nothing discovered yet.</p>
      )}
      {data && data.discovered.length > 0 && (
        <DiscoveredTable rows={data.discovered} busy={busy} onAct={act} />
      )}
    </div>
  )
}

function ErrorBox({ message }) {
  return (
    <div role="alert" className="text-sm p-3 rounded" style={{
      background: 'rgba(239,68,68,0.1)', color: '#ef4444',
      border: '1px solid rgba(239,68,68,0.3)',
    }}>{message}</div>
  )
}

function SourceErrors({ sources }) {
  const failing = sources.filter(s => s.last_error)
  if (failing.length === 0) return null
  return (
    <div data-testid="discovery-source-errors" className="text-sm"
      style={{ color: 'var(--yellow)' }}>
      {failing.map(s => (
        <p key={s.source}>
          Source {s.source}: {s.last_error} (previous candidates kept)
        </p>
      ))}
    </div>
  )
}

const HEADERS = ['Source', 'Database', 'Owner', 'Status', 'Expires', 'Last error', '']

function DiscoveredTable({ rows, busy, onAct }) {
  return (
    <div className="rounded-lg overflow-x-auto" style={{
      background: 'var(--bg-card)', border: '1px solid var(--border)',
    }}>
      <table className="w-full text-sm" data-testid="discovered-table">
        <thead><tr style={{ borderBottom: '1px solid var(--border)' }}>
          {HEADERS.map(h => (
            <th key={h} className="text-left px-4 py-3 text-xs whitespace-nowrap"
              style={{ color: 'var(--text-secondary)' }}>{h}</th>
          ))}
        </tr></thead>
        <tbody>{rows.map(row => (
          <tr key={row.id} style={{ borderBottom: '1px solid var(--border)' }}>
            <td className="px-4 py-3">{row.source}</td>
            <td className="px-4 py-3 font-mono">
              {row.dbname} <span style={{ color: 'var(--text-secondary)' }}>
                {row.host}:{row.port}</span>
            </td>
            <td className="px-4 py-3">{row.owner}</td>
            <td className="px-4 py-3">
              {row.status}{row.already_managed ? ' (already managed)' : ''}
            </td>
            <td className="px-4 py-3">
              {row.expires_at ? formatTime(row.expires_at) : ''}
            </td>
            <td className="px-4 py-3" style={{ color: 'var(--text-secondary)' }}>
              {row.last_error}
            </td>
            <td className="px-4 py-3 whitespace-nowrap">
              <RowActions row={row} busy={busy} onAct={onAct} />
            </td>
          </tr>
        ))}</tbody>
      </table>
    </div>
  )
}

function RowActions({ row, busy, onAct }) {
  const button = (action, label, body) => (
    <button key={action} data-testid={`${action}-${row.id}`}
      disabled={busy !== null} onClick={() => onAct(row.id, action, body)}
      className="ml-2 px-2 py-1 rounded text-xs" style={{
        background: 'var(--bg-card)', color: 'var(--text-primary)',
        border: '1px solid var(--border)',
      }}>{label}</button>
  )
  if (row.status === 'enrolled') {
    return [button('adopt', 'Adopt'),
      button('extend', `Extend ${EXTEND_HOURS}h`, { hours: EXTEND_HOURS })]
  }
  if (row.status === 'candidate' && !row.already_managed) {
    return button('ignore', 'Ignore')
  }
  return null
}
