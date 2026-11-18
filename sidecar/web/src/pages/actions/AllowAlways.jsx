import { useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'

// AllowAlways is the approval card's second affirmative button (ux-07a):
// "Approve, and stop asking for <class> on <database>". The server offers
// it (card.trust.allow_always) only when the class may take the L2 to L3
// step now and the caller is an admin; pressing it confirms first, then
// approves the action and the class's L3 promotion in one request.

// AllowAlwaysButton renders nothing unless the server offers the button.
export function AllowAlwaysButton({ card, busy, canApprove, onConfirm }) {
  const [open, setOpen] = useState(false)
  const trust = card.trust
  if (!trust?.allow_always) return null
  const label = trust.allow_always_label
    || `Approve, and stop asking for ${trust.class} on ${card.database}`
  const history = trust.approval_history
  const evidence = history?.approvals
    ? `You approved ${trust.class} ${history.approvals} times with no rollback. ` : ''
  return <>
    <button data-testid="approval-allow-always" disabled={busy || !canApprove}
      onClick={() => setOpen(true)} className="px-3 py-1 rounded text-sm"
      style={{ border: '1px solid var(--green-solid)', color: 'var(--text-primary)',
        opacity: busy || !canApprove ? 0.5 : 1 }}>
      {label}
    </button>
    <ConfirmDialog open={open} title={label} action="Approve, and stop asking"
      database={card.database} riskTier={card.risk?.tier} sql={card.sql} busy={busy}
      consequence={`${evidence}pg_sage runs this SQL now, then runs ${trust.class} on `
        + `${card.database} unattended (L3) without asking. A regression or a rollback `
        + 'demotes it and pg_sage asks again.'}
      testId="approval-allow-always-confirm"
      onConfirm={() => { onConfirm(); setOpen(false) }}
      onCancel={() => setOpen(false)} />
  </>
}
