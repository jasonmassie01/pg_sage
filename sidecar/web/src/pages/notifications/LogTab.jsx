import { useState, useEffect, useCallback } from 'react'
import { cardStyle, ErrorBanner } from './shared'

export function LogTab() {
  const [entries, setEntries] = useState([])
  const [error, setError] = useState(null)

  const fetchLog = useCallback(async () => {
    try {
      const res = await fetch(
        '/api/v1/notifications/log?limit=100',
        { credentials: 'include' })
      if (!res.ok) throw new Error('Failed to load log')
      const data = await res.json()
      setEntries(data.log || [])
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => {
    fetchLog()
    const id = setInterval(fetchLog, 30000)
    return () => clearInterval(id)
  }, [fetchLog])

  return (
    <div>
      {error && <ErrorBanner msg={error} />}
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
                {['Event', 'Subject', 'Status', 'Error',
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
                  <td className="px-4 py-2"
                    style={{
                      color: 'var(--text-primary)',
                    }}>
                    {e.subject}
                  </td>
                  <td className="px-4 py-2">
                    <span style={{
                      color: e.status === 'sent'
                        ? '#22c55e' : '#ef4444',
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
