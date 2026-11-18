import { useState } from 'react'

// Waiting promotions (ux-06b): every class pg_sage proposes to raise, on
// every database the page shows, with the one Approve (admins) and
// Reject (admins and operators). The server refuses a second approval of
// the same proposal; the page then re-reads and the row is gone.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

// waitingOf lists the pending proposals of every database's rows.
function waitingOf(databases) {
  return (Array.isArray(databases) ? databases : []).flatMap(view =>
    (view.rows || []).filter(row => row.pending?.id)
      .map(row => ({ database: view.database, row, pending: row.pending })))
}

export function WaitingPromotions({ databases, isAdmin, canReject, onApprove, onReject }) {
  const [note, setNote] = useState('')
  const waiting = waitingOf(databases)
  return (
    <div className="rounded border p-3 space-y-2" style={card}
      data-testid="autonomy-waiting">
      <h3 className="text-sm font-semibold" style={strong}>Waiting promotions</h3>
      {waiting.length === 0 && (
        <div className="text-xs" style={muted} data-testid="autonomy-waiting-empty">
          Nothing waits: pg_sage proposes a promotion once a class&apos;s evidence meets
          the next level.
        </div>
      )}
      {waiting.length > 0 && (isAdmin || canReject) && (
        <label className="block text-xs" style={muted}>
          Note
          <input className="ml-2 rounded px-2 py-1 text-sm" value={note}
            data-testid="autonomy-waiting-note" onChange={e => setNote(e.target.value)} />
        </label>
      )}
      {waiting.map(({ database, row, pending }) => (
        <div key={`${database}-${pending.id}`}
          className="flex flex-wrap items-center gap-3 text-sm" data-testid={`autonomy-waiting-${database}-${row.family}-${row.class}`}>
          <span style={strong}>
            {database}: {row.family} / {row.class}, {pending.from || row.level} to {pending.to}
          </span>
          {pending.expires_at && (
            <span className="text-xs" style={muted}>expires {pending.expires_at}</span>
          )}
          {isAdmin && (
            <button type="button" className="rounded px-2 py-1 text-xs"
              style={{ border: '1px solid var(--green)', color: 'var(--green)' }}
              onClick={() => onApprove(database, pending.id, note)}>Approve</button>
          )}
          {canReject && (
            <button type="button" className="rounded px-2 py-1 text-xs"
              style={{ border: '1px solid var(--border)' }}
              onClick={() => onReject(database, pending.id, note)}>Reject</button>
          )}
        </div>
      ))}
    </div>
  )
}
