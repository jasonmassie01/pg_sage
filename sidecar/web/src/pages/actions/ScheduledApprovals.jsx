import { useState } from 'react'
import { scheduledLabel } from './scheduledLabel'

// ScheduledApprovals lists approvals waiting for their maintenance window
// (policy-04): when each will run, and a Cancel that returns it to
// pending. pg_sage asks the policy gate again when the window opens.

export function ScheduledApprovals({ rows, onChanged }) {
  const [error, setError] = useState(null)
  if (!Array.isArray(rows) || rows.length === 0) return null

  async function cancel(row) {
    setError(null)
    const db = row.database_name ? `?database=${encodeURIComponent(row.database_name)}` : ''
    try {
      const res = await fetch(`/api/v1/actions/${row.id}/unschedule${db}`, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
      })
      const body = await res.json().catch(() => ({}))
      if (!res.ok) {
        setError(body.error || `Cancel failed (${res.status})`)
        return
      }
      onChanged?.()
    } catch (err) {
      setError(`Cancel failed: ${err.message}`)
    }
  }

  return (
    <div data-testid="scheduled-approvals" className="space-y-2">
      <div className="text-sm font-medium" style={{ color: 'var(--text-primary)' }}>
        Approved, waiting for the maintenance window
      </div>
      {error && <div role="alert" className="text-sm" style={{ color: 'var(--red)' }}>
        {error}</div>}
      {rows.map(row => (
        <div key={`${row.database_name || ''}:${row.id}`}
          className="flex flex-wrap items-center gap-3 p-2 rounded text-sm"
          style={{ border: '1px solid var(--border)' }}>
          <span style={{ color: 'var(--yellow)' }}>Scheduled for {scheduledLabel(row)}</span>
          {row.database_name && <span style={{ color: 'var(--text-secondary)' }}>
            {row.database_name}</span>}
          <code className="text-xs break-all">{row.proposed_sql}</code>
          <button onClick={() => cancel(row)} aria-label={`Cancel schedule of action ${row.id}`}
            className="px-2 py-1 rounded text-xs"
            style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
            Cancel
          </button>
        </div>
      ))}
    </div>
  )
}
