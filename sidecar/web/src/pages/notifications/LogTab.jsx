import { useState, useEffect, useCallback } from 'react'
import { ErrorBanner } from '../../components/ErrorBanner'
import { cardStyle, DELIVERY_STATUS_COLORS as STATUS_COLORS } from './shared'

// logURL is the log read for the selected database; the fleet-wide
// scope ('all') or none lists every database (alerting-06c).
function logURL(database) {
  const params = new URLSearchParams({ limit: '100' })
  if (database && database !== 'all') params.set('database', database)
  return `/api/v1/notifications/log?${params.toString()}`
}

export function LogTab({ database } = {}) {
  const [entries, setEntries] = useState([])
  const [error, setError] = useState(null)

  const fetchLog = useCallback(async () => {
    try {
      const res = await fetch(logURL(database), { credentials: 'include' })
      if (!res.ok) throw new Error('Failed to load log')
      const data = await res.json()
      setEntries(data.log || [])
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [database])

  useEffect(() => {
    fetchLog()
    const id = setInterval(fetchLog, 30000)
    return () => clearInterval(id)
  }, [fetchLog])

  return (
    <div>
      {error && <div className="mb-4"><ErrorBanner message={error} /></div>}
      {!entries.length ? (
        <div className="text-sm p-4"
          style={{ color: 'var(--text-secondary)' }}>
          No notifications sent yet.
        </div>
      ) : (
        <div className="rounded-lg overflow-hidden"
          style={cardStyle}>
          <table className="w-full text-sm">
            <thead>
              <tr style={{
                borderBottom: '1px solid var(--border)',
              }}>
                {['Event', 'Database', 'Subject', 'Status', 'Error',
                  'Sent'].map(h => (
                  <th key={h}
                    className="text-left px-4 py-2 text-xs"
                    style={{
                      color: 'var(--text-secondary)',
                    }}>
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {entries.map(e => (
                <tr key={e.id} style={{
                  borderBottom: '1px solid var(--border)',
                }}>
                  <td className="px-4 py-2"
                    style={{
                      color: 'var(--text-primary)',
                    }}>
                    {e.event}
                  </td>
                  <td className="px-4 py-2 text-xs"
                    data-testid={`notify-database-${e.id}`}
                    style={{ color: 'var(--text-secondary)' }}>
                    {e.database || 'unknown'}
                  </td>
                  <td className="px-4 py-2"
                    style={{
                      color: 'var(--text-primary)',
                    }}>
                    {e.subject}
                  </td>
                  <td className="px-4 py-2">
                    <span data-testid={`notify-status-${e.id}`} style={{
                      color: STATUS_COLORS[e.status]
                        || 'var(--text-secondary)',
                    }}>
                      {e.status}
                    </span>
                  </td>
                  <td className="px-4 py-2 text-xs"
                    style={{
                      color: 'var(--text-secondary)',
                    }}>
                    {e.error || '-'}
                  </td>
                  <td className="px-4 py-2 text-xs"
                    style={{
                      color: 'var(--text-secondary)',
                    }}>
                    {new Date(e.sent_at).toLocaleString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
