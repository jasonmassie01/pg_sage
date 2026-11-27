import { useId, useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { canRollBackRow, rollbackButtonLabel, rollbackUnavailableReason } from './ledger'
import { rowDatabase } from './workflow'

// Rollback from the Actions ledger (dashboard-10). The button opens a
// ConfirmDialog that shows the action id, target database, exact rollback SQL
// and risk tier; nothing is sent until Confirm, and a failure stays in the
// open dialog. The request body is {reason, record_kind}, the reason now
// typed by the operator (ux-01b) instead of a fixed string.

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

// postRollback sends today's body with the operator's reason. surface
// names the page the decision came from (X-Sage-Surface, ux-01a).
async function postRollback(row, reason, surface) {
  const dbParam = row.database_name
    ? `?database=${encodeURIComponent(row.database_name)}` : ''
  const headers = { 'Content-Type': 'application/json' }
  if (surface) headers['X-Sage-Surface'] = surface
  const res = await fetch(`/api/v1/actions/${row.id}/rollback${dbParam}`, {
    method: 'POST',
    credentials: 'include',
    headers,
    body: JSON.stringify({ reason, record_kind: row.record_kind }),
  })
  let json = {}
  try { json = await res.json() } catch { /* non-JSON error body */ }
  if (!res.ok || !json.ok) throw new Error(json.error || `Rollback failed (${res.status})`)
}

// RollbackDialog asks why (ux-01b): the reason is kept with the rollback,
// so the ledger and the trust model learn it. onDone receives the row and
// the reason sent.
export function RollbackDialog({ row, database, riskTier, surface, onClose, onDone }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function runRollback(reason) {
    setBusy(true)
    setError(null)
    try {
      await postRollback(row, reason, surface)
      onDone(row, reason)
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
      reasonLabel="Why roll it back? (3 to 500 characters, kept with the rollback)"
      busy={busy} error={error} onConfirm={runRollback} onCancel={onClose} />
  )
}
