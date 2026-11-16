import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ApprovalCard } from '../actions/ApprovalCard'
import { decideFact } from '../../lib/facts'
import { kindLabel, withSurface } from '../../lib/inbox'
import { riskLabel } from '../../lib/riskTier'

// NeedsYou is the Inbox's section (a): ux-01a's items, newest risk first as
// the API sorts them. Each is decided in place through its own route with
// X-Sage-Surface: inbox, or opened on its own page (Href).

const SURFACE = 'inbox'
const muted = { color: 'var(--text-secondary)' }
const button = 'px-3 py-1 rounded text-sm'
// The kinds only an admin decides (ux-01a inbox.DeciderRole).
const ADMIN_KINDS = new Set(['autonomy_promotion', 'runbook_signature', 'policy_exception'])

// safeHref keeps an in-app hash link; anything else is not a link.
const safeHref = href => (typeof href === 'string' && href.startsWith('#/') ? href : null)
const dbQuery = db => `?database=${encodeURIComponent(db)}`

// InlineApproval loads the item's approval card and decides it in place.
function InlineApproval({ item, onDecided }) {
  const { data, error } = useAPI(`/api/v1/approvals/${encodeURIComponent(item.id)}`
    + dbQuery(item.database), 0)
  if (error) return <p className="text-sm" style={{ color: 'var(--red)' }}>{error}</p>
  if (!data?.card) return <p className="text-sm" style={muted}>Loading the approval card…</p>
  return <ApprovalCard card={data.card} onDecided={onDecided} surface={SURFACE}
    eligible={data.eligible} blockedReason={data.defer_reason || data.blocked_reason}
    windowOnly={data.policy_detail?.window_only === true} />
}

async function approvePromotion(item) {
  const res = await fetch(`/api/v1/sre/autonomy/proposals/${encodeURIComponent(item.id)}`
    + `/approve${dbQuery(item.database)}`, {
    method: 'POST', credentials: 'include',
    headers: withSurface({ 'Content-Type': 'application/json' }, SURFACE),
    body: JSON.stringify({ note: 'approved from the Inbox' }),
  })
  if (!res.ok) {
    let message = `Approve failed (${res.status})`
    try { message = (await res.json()).error || message } catch { /* keep the status */ }
    throw new Error(message)
  }
}

// PromotionApproval asks before granting more autonomy; a HIGH one needs
// the typed phrase, like every destructive confirm.
function PromotionApproval({ item, onDecided }) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function approve() {
    setBusy(true)
    setError(null)
    try {
      await approvePromotion(item)
      setOpen(false)
      onDecided()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return <>
    <button type="button" className={button} onClick={() => setOpen(true)}
      style={{ background: 'var(--green)', color: '#fff' }}>Approve</button>
    <ConfirmDialog open={open} title={`Approve: ${item.title}`} action="Approve"
      database={item.database} riskTier={item.risk} busy={busy} error={error}
      consequence="pg_sage may then run this class of change on its own on this database."
      onConfirm={approve} onCancel={() => setOpen(false)} />
  </>
}

// FactDecision confirms or rejects a proposed fact in place.
function FactDecision({ item, onDecided }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function decide(verb) {
    setBusy(true)
    setError(null)
    try {
      await decideFact(item.database, item.id, verb,
        verb === 'reject' ? 'rejected from the Inbox' : '', SURFACE)
      onDecided()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return <>
    <button type="button" className={button} disabled={busy} onClick={() => decide('confirm')}
      style={{ background: 'var(--green)', color: '#fff' }}>Confirm</button>
    <button type="button" className={button} disabled={busy} onClick={() => decide('reject')}
      style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>Reject</button>
    {error && <span role="alert" className="text-sm" style={{ color: 'var(--red)' }}>{error}</span>}
  </>
}

function Decision({ item, onDecided, reviewing, onReview }) {
  if (!item.actionable) {
    return <p className="text-xs" style={muted}>Your role can view this;{' '}
      {ADMIN_KINDS.has(item.kind) ? 'an admin' : 'an operator or admin'} decides this.</p>
  }
  if (item.kind === 'change_approval') {
    return <button type="button" className={button} aria-expanded={reviewing}
      onClick={onReview} style={{ border: '1px solid var(--accent)', color: 'var(--accent)' }}>
      {reviewing ? 'Hide review' : 'Review'}</button>
  }
  if (item.kind === 'fact') return <FactDecision item={item} onDecided={onDecided} />
  if (item.kind === 'autonomy_promotion') {
    return <PromotionApproval item={item} onDecided={onDecided} />
  }
  return <p className="text-xs" style={muted}>Decide this on its page.</p>
}

function InboxItem({ item, onDecided }) {
  const [reviewing, setReviewing] = useState(false)
  const href = safeHref(item.href)
  return (
    <li data-testid={`inbox-item-${item.kind}-${item.id}`} className="rounded border p-3 space-y-2"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <div className="text-xs" style={muted}>
        {kindLabel(item.kind)} · {item.database} · Risk: {riskLabel(item.risk) || item.risk}
      </div>
      <div className="font-medium" style={{ color: 'var(--text-primary)' }}>{item.title}</div>
      {item.why && <p className="text-sm" style={muted}>{item.why}</p>}
      <div className="flex flex-wrap items-center gap-2">
        <Decision item={item} onDecided={onDecided} reviewing={reviewing}
          onReview={() => setReviewing(open => !open)} />
        {href && <a href={href} className="text-sm underline" style={{ color: 'var(--accent)' }}>
          Open</a>}
      </div>
      {reviewing && <InlineApproval item={item} onDecided={() => {
        setReviewing(false)
        onDecided()
      }} />}
    </li>
  )
}

export function NeedsYou({ items, onDecided }) {
  return (
    <ul className="space-y-3" data-testid="inbox-needs-you">
      {items.map(item => <InboxItem key={`${item.kind}:${item.database}:${item.id}`}
        item={item} onDecided={onDecided} />)}
    </ul>
  )
}
