import { useCallback, useMemo, useRef, useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { useToast } from '../../components/Toast'
import { apiGet } from '../../lib/api'
import { ApprovalCard } from '../actions/ApprovalCard'
import { BulkApproveBar } from '../actions/BulkApproveBar'
import {
  SAFE_ONLY, bulkKey, classGroups, isSafeTier, useBulkSelection,
} from '../actions/bulkApprove'
import { useApprovalKeys } from '../actions/useApprovalKeys'
import { decideFact } from '../../lib/facts'
import { kindLabel, withSurface } from '../../lib/inbox'
import { riskLabel } from '../../lib/riskTier'

// NeedsYou is the Inbox's section (a): ux-01a's items, newest risk first as
// the API sorts them. Each is decided in place through its own route with
// X-Sage-Surface: inbox, or opened on its own page (Href). Safe-tier change
// approvals can be marked and approved together, and the approval keys
// (j, k, a, r, s, x, Enter) drive the list (ux-07b).

const SURFACE = 'inbox'
const muted = { color: 'var(--text-secondary)' }
const button = 'px-3 py-1 rounded text-sm'
// The kinds only an admin decides (ux-01a inbox.DeciderRole).
const ADMIN_KINDS = new Set(['autonomy_promotion', 'runbook_signature', 'policy_exception'])

// safeHref keeps an in-app hash link; anything else is not a link.
const safeHref = href => (typeof href === 'string' && href.startsWith('#/') ? href : null)
const dbQuery = db => `?database=${encodeURIComponent(db)}`

// InlineApproval loads the item's approval card and decides it in place;
// actionsRef receives the card's key verbs.
function InlineApproval({ item, onDecided, actionsRef }) {
  const { data, error } = useAPI(`/api/v1/approvals/${encodeURIComponent(item.id)}`
    + dbQuery(item.database), 0)
  if (error) return <p className="text-sm" style={{ color: 'var(--red)' }}>{error}</p>
  if (!data?.card) return <p className="text-sm" style={muted}>Loading the approval card…</p>
  return <ApprovalCard card={data.card} onDecided={onDecided} surface={SURFACE}
    eligible={data.eligible} blockedReason={data.defer_reason || data.blocked_reason}
    windowOnly={data.policy_detail?.window_only === true} actionsRef={actionsRef} />
}

// A G-M1 model gate promotion (review-05a) is "review-gate-<id>" from
// sage.review_gate and is approved on its own route; every other promotion
// is the SRE autonomy ledger's.
const REVIEW_GATE = /^review-gate-([1-9][0-9]*)$/

function promotionURL(item) {
  if (!String(item.id).startsWith('review-gate-')) {
    return `/api/v1/sre/autonomy/proposals/${encodeURIComponent(item.id)}`
      + `/approve${dbQuery(item.database)}`
  }
  const m = REVIEW_GATE.exec(item.id)
  if (!m) throw new Error(`${item.id} is not a valid model gate id`)
  return `/api/v1/databases/${encodeURIComponent(item.database)}/review/gate/${m[1]}/approve`
}

async function approvePromotion(item) {
  const res = await fetch(promotionURL(item), {
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
      style={{ background: 'var(--green-solid)', color: '#fff' }}>Approve</button>
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
      style={{ background: 'var(--green-solid)', color: '#fff' }}>Confirm</button>
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

const itemKey = item => `${item.kind}:${item.database}:${item.id}`
const isChange = item => item.kind === 'change_approval' && item.actionable
const bulkable = item => isChange(item) && isSafeTier(item.risk)
const bulkKeyOf = item => bulkKey(item.database, item.id)

// useItemVerbs answers the list's keys for one item: Enter opens its review
// (or its page); a, r and s open the review first, then reach the card.
function useItemVerbs(item, setReviewing) {
  const card = useRef(null)
  const pending = useRef(null)
  const onCard = useCallback(handle => {
    card.current = handle
    if (handle && pending.current) {
      const verb = pending.current
      pending.current = null
      handle[verb]?.()
    }
  }, [])
  const onVerb = verb => {
    const href = safeHref(item.href)
    if (verb === 'open' && isChange(item)) setReviewing(open => !open)
    else if (verb === 'open' && href) window.location.hash = href
    else if (verb === 'open' || !isChange(item)) return
    else if (card.current) card.current[verb]?.()
    else {
      pending.current = verb
      setReviewing(true)
    }
  }
  return { onCard, onVerb }
}

function BulkBox({ item, checked, onToggle }) {
  if (!isChange(item)) return null
  const safe = bulkable(item)
  return <label className="inline-flex items-center gap-2 text-xs" style={muted}>
    <input type="checkbox" data-testid={`bulk-select-${item.database}-${item.id}`}
      checked={checked} disabled={!safe} onChange={onToggle}
      aria-label={`Select ${item.title} for bulk approve`} />
    {safe ? 'Select for bulk approve' : 'Bulk approve is safe tier only'}
  </label>
}

function itemStyle(selected, keyed) {
  const style = { borderColor: 'var(--border)', background: 'var(--bg-card)' }
  return selected && keyed
    ? { ...style, outline: '2px solid var(--accent)', outlineOffset: '2px' } : style
}

// The item's decision row is the phone's sticky action bar (ux-12), except
// while its review is open: then the approval card's own bar is the one.
const BAR = 'approval-action-bar '
const ROW = 'flex flex-wrap items-center gap-2'

function InboxItem({ item, onDecided, verbsRef, selected, keyed, checked, onToggle }) {
  const [reviewing, setReviewing] = useState(false)
  const verbs = useItemVerbs(item, setReviewing)
  verbsRef(verbs.onVerb)
  const href = safeHref(item.href)
  return (
    <li data-testid={`inbox-item-${item.kind}-${item.id}`} className="rounded border p-3 space-y-2"
      data-selected={selected ? 'true' : undefined} aria-current={selected || undefined}
      style={itemStyle(selected, keyed)}>
      <BulkBox item={item} checked={checked} onToggle={onToggle} />
      <div className="text-xs" style={muted}>
        {kindLabel(item.kind)} · {item.database} · Risk: {riskLabel(item.risk) || item.risk}
      </div>
      <div className="font-medium" style={{ color: 'var(--text-primary)' }}>{item.title}</div>
      {item.why && <p className="text-sm" style={muted}>{item.why}</p>}
      <div data-testid="inbox-decision-bar" className={`${reviewing ? '' : BAR}${ROW}`}>
        <Decision item={item} onDecided={onDecided} reviewing={reviewing}
          onReview={() => setReviewing(open => !open)} />
        {href && <a href={href} className="text-sm underline" style={{ color: 'var(--accent)' }}>
          Open</a>}
      </div>
      {reviewing && <InlineApproval item={item} actionsRef={verbs.onCard} onDecided={() => {
        setReviewing(false)
        onDecided()
      }} />}
    </li>
  )
}

// loadInboxCards reads each marked item's card, so the confirm shows (and
// the request binds) the card as it is now; one no longer safe is left out.
async function loadInboxCards(marked) {
  const cards = []
  const skipped = []
  await Promise.all(marked.map(async (item, i) => {
    const key = bulkKeyOf(item)
    try {
      const { card } = await apiGet(`/api/v1/approvals/${encodeURIComponent(item.id)}`
        + dbQuery(item.database))
      if (isSafeTier(card?.risk?.tier)) cards[i] = card
      else {
        skipped.push({ key, title: card?.title || item.title,
          reason: `not safe tier on its card (${card?.risk?.tier || 'unknown'})` })
      }
    } catch (err) {
      skipped.push({ key, title: item.title, reason: err.message })
    }
  }))
  return { cards: cards.filter(Boolean), skipped }
}

// useInboxBulk is the list's bulk selection and the bar's inputs.
function useInboxBulk(items, onDecided) {
  const changes = useMemo(() => items.filter(bulkable), [items])
  const keys = useMemo(() => changes.map(bulkKeyOf), [changes])
  const selection = useBulkSelection(keys)
  const groups = useMemo(() => classGroups(changes.map(it => ({ key: bulkKeyOf(it),
    cls: it.class, database: it.database, safe: true }))), [changes])
  return {
    changes, selection, groups,
    loadCards: () => loadInboxCards(changes.filter(it => selection.selected.has(bulkKeyOf(it)))),
    labelFor: r => changes.find(it => bulkKeyOf(it) === bulkKey(r.database, r.id))?.title,
    onSettled: body => {
      selection.settle(body?.results)
      onDecided()
    },
  }
}

// useInboxKeys wires the approval keys to the items: x marks a safe change
// approval, and a with items marked opens the bulk confirm.
function useInboxKeys(items, bulk, bulkRef) {
  const verbs = useRef(new Map())
  const toast = useToast()
  const onVerb = useCallback((verb, index) => {
    const item = items[index]
    if (verb === 'toggle') {
      if (bulkable(item)) bulk.selection.toggle(bulkKeyOf(item))
      else toast.error(`${item.title}: ${SAFE_ONLY}`)
    } else if (verb === 'approve' && bulk.selection.selected.size > 0) {
      bulkRef.current?.open()
    } else {
      verbs.current.get(itemKey(item))?.(verb)
    }
  }, [items, bulk, bulkRef, toast])
  const keys = useApprovalKeys(items.length, onVerb)
  const verbsRef = key => fn => verbs.current.set(key, fn)
  return { ...keys, verbsRef }
}

export function NeedsYou({ items, onDecided }) {
  const bulkRef = useRef(null)
  const bulk = useInboxBulk(items, onDecided)
  const keys = useInboxKeys(items, bulk, bulkRef)
  return <div className="space-y-2">
    {bulk.changes.length > 0 && <BulkApproveBar count={bulk.selection.selected.size}
      groups={bulk.groups} onSelectGroup={bulk.selection.add} loadCards={bulk.loadCards}
      surface={SURFACE} onSettled={bulk.onSettled} labelFor={bulk.labelFor}
      controlRef={bulkRef} />}
    <ul className="space-y-3" data-testid="inbox-needs-you">
      {items.map((item, i) => <InboxItem key={itemKey(item)} item={item}
        onDecided={onDecided} verbsRef={keys.verbsRef(itemKey(item))}
        selected={i === keys.selected} keyed={keys.used}
        checked={bulk.selection.selected.has(bulkKeyOf(item))}
        onToggle={() => bulk.selection.toggle(bulkKeyOf(item))} />)}
    </ul>
  </div>
}
