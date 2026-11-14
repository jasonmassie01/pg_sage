import { useId, useState } from 'react'
import { Dialog } from './Dialog'
import { SQLBlock } from './SQLBlock'
import { isHighRisk, riskLabel } from '../lib/riskTier'

// ConfirmDialog asks before a destructive action (dashboard-10). It names the
// target database, the risk tier, what will happen and the exact SQL. A HIGH
// tier confirm stays disabled until the operator types the phrase exactly
// (the database name by default); with no phrase it fails closed on
// "confirm". An error from the action shows inside the open dialog.

const FALLBACK_PHRASE = 'confirm'

function Detail({ label, testId, children }) {
  return (
    <div className="text-sm" data-testid={testId}>
      <span style={{ color: 'var(--text-secondary)' }}>{label}: </span>
      <span className="font-medium">{children}</span>
    </div>
  )
}

// PhraseInput is the typed-phrase field; the trust escalation confirm reuses
// it with autoFocus off so its initial focus stays on Cancel (dashboard-10c).
export function PhraseInput({ phrase, value, onChange, testId, autoFocus = true }) {
  const id = useId()
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-sm" style={{ color: 'var(--text-secondary)' }}>
        Type <code className="font-mono">{phrase}</code> to confirm
      </label>
      <input id={id} data-testid={testId} data-autofocus={autoFocus || undefined} value={value}
        onChange={e => onChange(e.target.value)} autoComplete="off" spellCheck={false}
        className="w-full rounded px-2 py-1 text-sm font-mono"
        style={{
          background: 'var(--bg-primary)', color: 'var(--text-primary)',
          border: '1px solid var(--border)',
        }} />
    </div>
  )
}

function Buttons({ action, danger, canConfirm, busy, onCancel, onConfirm, testId }) {
  return (
    <div className="flex justify-end gap-2 pt-2">
      <button type="button" data-testid={`${testId}-cancel`} data-autofocus
        disabled={busy} onClick={onCancel} className="px-3 py-1.5 rounded text-sm"
        style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
        Cancel
      </button>
      <button type="button" data-testid={`${testId}-confirm`}
        disabled={!canConfirm || busy} onClick={onConfirm}
        className="px-3 py-1.5 rounded text-sm font-medium disabled:opacity-50"
        style={{ background: danger ? 'var(--red)' : 'var(--accent)', color: '#fff' }}>
        {busy ? 'Working…' : action}
      </button>
    </div>
  )
}

// ConfirmBody mounts only while the dialog is open, so a reopened dialog
// starts with an empty phrase.
function ConfirmBody({
  action, database, sql, riskTier, consequence, confirmPhrase, busy, error,
  onConfirm, onCancel, testId,
}) {
  const [typed, setTyped] = useState('')
  const high = isHighRisk(riskTier)
  const phrase = confirmPhrase || database || FALLBACK_PHRASE
  const canConfirm = !high || typed === phrase
  return (
    <>
      {consequence && <p className="text-sm">{consequence}</p>}
      {database && <Detail label="Database" testId={`${testId}-database`}>{database}</Detail>}
      {riskTier && (
        <Detail label="Risk" testId={`${testId}-risk`}>{riskLabel(riskTier)}</Detail>
      )}
      {sql && <div data-testid={`${testId}-sql`}><SQLBlock sql={sql} /></div>}
      {high && (
        <PhraseInput phrase={phrase} value={typed} onChange={setTyped}
          testId={`${testId}-phrase`} />
      )}
      {error && (
        <div role="alert" data-testid={`${testId}-error`} className="text-sm p-2 rounded"
          style={{ color: 'var(--red)', border: '1px solid var(--red)' }}>
          {error}
        </div>
      )}
      <Buttons action={action} danger={high} canConfirm={canConfirm} busy={busy}
        onCancel={onCancel} onConfirm={onConfirm} testId={testId} />
    </>
  )
}

export function ConfirmDialog({
  open, title, action = 'Confirm', busy = false, onCancel, testId = 'confirm-dialog', ...body
}) {
  return (
    <Dialog open={open} title={title} onClose={onCancel} busy={busy} testId={testId}>
      <ConfirmBody action={action} busy={busy} onCancel={onCancel} testId={testId} {...body} />
    </Dialog>
  )
}
