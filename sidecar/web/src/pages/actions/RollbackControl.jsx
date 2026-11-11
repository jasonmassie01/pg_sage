import { useId, useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { canRollBackRow, rollbackButtonLabel, rollbackUnavailableReason } from './ledger'
import { rowDatabase } from './workflow'

// Rollback from the Actions ledger (dashboard-10). The button opens a
// ConfirmDialog that shows the action id, target database, exact rollback SQL
// and risk tier; nothing is sent until Confirm, and a failure stays in the
// open dialog. The request body is unchanged from the window.confirm flow.

const buttonStyle = { background: 'var(--yellow)', color: '#111827' }

export function RollbackButton({ row, onOpen }) {
  const reasonId = useId()
  const reason = rollbackUnavailableReason(row)
  if (reason) {
    return (
      <div className="space-y-1">
        <button type="button" data-testid="rollback-action-button" disabled
          aria-describedby={reasonId} className="px-2 py-1 rounded text-xs opacity-50"
          style={buttonStyle}>
          {rollbackButtonLabel(row)}
        </button>
        <p id={reasonId} data-testid="rollback-unavailable-reason" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          {reason}
        </p>
      </div>
    )
  }
  if (!canRollBackRow(row)) return null
  return (
    <button type="button" data-testid="rollback-action-button" onClick={() => onOpen(row)}
      className="mt-2 px-2 py-1 rounded text-xs" style={buttonStyle}>
      {rollbackButtonLabel(row)}
    </button>
  )
}

async function postRollback(row) {
  const dbParam = row.database_name
    ? `?database=${encodeURIComponent(row.database_name)}` : ''
  const res = await fetch(`/api/v1/actions/${row.id}/rollback${dbParam}`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      reason: 'manual rollback from actions UI',
      record_kind: row.record_kind,
    }),
  })
  let json = {}
  try { json = await res.json() } catch { /* non-JSON error body */ }
  if (!res.ok || !json.ok) throw new Error(json.error || `Rollback failed (${res.status})`)
}

export function RollbackDialog({ row, database, riskTier, onClose, onDone }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function runRollback() {
    setBusy(true)
    setError(null)
    try {
      await postRollback(row)
      onDone(row)
    } catch (err) {
      // A destructive rollback must never fail silently: the operator sees
      // why in the dialog they confirmed, not in a toast behind it.
      setError(err.message || 'Rollback request failed')
    } finally {
      setBusy(false)
    }
  }
  const target = row ? rowDatabase(row, database) : ''
  return (
    <ConfirmDialog open={Boolean(row)} testId="confirm-dialog"
      title={row ? `Roll back action ${row.id}` : ''} action="Roll back"
      database={target} riskTier={riskTier} sql={row?.rollback_sql}
      confirmPhrase={target || (row ? `action ${row.id}` : '')}
      consequence="pg_sage runs the stored rollback SQL below on this database now."
      busy={busy} error={error} onConfirm={runRollback} onCancel={onClose} />
  )
}
