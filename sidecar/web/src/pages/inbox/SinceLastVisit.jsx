import { useState } from 'react'
import { ErrorBanner } from '../../components/ErrorBanner'
import { useToast } from '../../components/Toast'
import { apiGet } from '../../lib/api'
import { RollbackDialog } from '../actions/RollbackControl'
import { actionRisk } from '../actions/actionFormat'
import { ROLLBACK_LABELS as ROLLBACK, VERDICT_LABELS as VERDICTS } from '../../lib/outcomes'

// SinceLastVisit is the Inbox's section (b): the outcome ledger's rows
// (ux-02a) since the person was last here, with Undo on a row whose
// rollback is available. Undo loads the action record so the dialog shows
// the exact stored rollback SQL and risk, then asks why.

const muted = { color: 'var(--text-secondary)' }
const rowKey = row => `${row.database}-${row.action_id}`

function RollbackState({ row, reason }) {
  if (row.rollback_state === 'rolled_back' && reason) {
    return <span>Rolled back: {reason}</span>
  }
  return <span>{ROLLBACK[row.rollback_state] || row.rollback_state}</span>
}

function ChangeRow({ row, canUndo, reason, onUndo }) {
  return (
    <li data-testid={`inbox-change-${rowKey(row)}`} className="rounded border p-3 text-sm"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="font-medium" style={{ color: 'var(--text-primary)' }}>{row.what}</span>
        <span className="text-xs" style={muted}>
          {row.database} · {new Date(row.executed_at).toLocaleString()}
        </span>
      </div>
      <div className="mt-1 flex flex-wrap items-center gap-3 text-xs" style={muted}>
        <span>{VERDICTS[row.verdict] || row.verdict}</span>
        <RollbackState row={row} reason={reason} />
        <a href={`#/actions?action=${encodeURIComponent(row.action_id)}`} className="underline"
          style={{ color: 'var(--accent)' }}>Open action {row.action_id}</a>
        {canUndo && row.rollback_state === 'available' && (
          <button type="button" onClick={() => onUndo(row)} className="px-2 py-1 rounded text-xs"
            style={{ background: 'var(--yellow)', color: '#111827' }}>Undo</button>
        )}
      </div>
    </li>
  )
}

// useUndo opens the rollback dialog for a ledger row from its action record.
function useUndo(onRolledBack) {
  const toast = useToast()
  const [dialog, setDialog] = useState(null)
  const [loadError, setLoadError] = useState(null)
  const [reasons, setReasons] = useState({})
  async function open(row) {
    setLoadError(null)
    try {
      const detail = await apiGet(`/api/v1/actions/${encodeURIComponent(row.action_id)}`
        + `?database=${encodeURIComponent(row.database)}`)
      setDialog({ key: rowKey(row), row: { ...detail, id: row.action_id,
        database_name: row.database, record_kind: 'executed' } })
    } catch (err) {
      setLoadError(`Could not load action ${row.action_id} to roll it back: ${err.message}`)
    }
  }
  function done(row, reason) {
    setReasons(prev => ({ ...prev, [dialog.key]: reason }))
    setDialog(null)
    toast.success(`Action ${row.id} rolled back`)
    onRolledBack()
  }
  return { dialog, loadError, reasons, open, done, close: () => setDialog(null) }
}

// LedgerGaps names what the list may miss (lib/inbox ledgerGaps).
function LedgerGaps({ gaps, shown }) {
  if (!gaps?.partial) return null
  const parts = []
  if (gaps.failed.length) parts.push(`Could not read the ledger of: ${gaps.failed.join(', ')}.`)
  if (gaps.cut) parts.push(`Showing the newest ${shown} changes; the Ledger page has the rest.`)
  return <p role="status" data-testid="inbox-ledger-partial" className="text-xs"
    style={{ color: 'var(--yellow)' }}>{parts.join(' ')}</p>
}

export function SinceLastVisit({ changes, label, query, canUndo, gaps }) {
  const undo = useUndo(() => query.refetch?.())
  return (
    <section className="space-y-2" aria-label="Since you were last here">
      <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
        Since you were last here</h2>
      <p className="text-xs" data-testid="inbox-since-label" style={muted}>{label}</p>
      {query.error && <ErrorBanner message={`The outcome ledger could not be read: ${query.error}`}
        onRetry={query.refetch} />}
      {undo.loadError && <ErrorBanner message={undo.loadError} />}
      <LedgerGaps gaps={gaps} shown={changes.length} />
      {changes.length === 0 && !query.error
        ? <p className="text-sm" style={muted}>No changes in this window.</p>
        : <ul className="space-y-2">
          {changes.map(row => <ChangeRow key={rowKey(row)} row={row} canUndo={canUndo}
            reason={undo.reasons[rowKey(row)]} onUndo={undo.open} />)}
        </ul>}
      <RollbackDialog key={undo.dialog?.key || 'closed'} row={undo.dialog?.row || null}
        database={undo.dialog?.row?.database_name} surface="inbox"
        riskTier={undo.dialog ? actionRisk(undo.dialog.row) : undefined}
        onClose={undo.close} onDone={undo.done} />
    </section>
  )
}
