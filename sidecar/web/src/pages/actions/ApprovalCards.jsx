import { useCallback, useEffect, useMemo, useRef } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { useToast } from '../../components/Toast'
import { isFocusedQueue } from './actionFocus'
import { EmptyState } from '../../components/EmptyState'
import { FactBadges } from '../../components/FactBadges'
import { ApprovalCard } from './ApprovalCard'
import { PendingErrors } from './PendingErrors'
import { caseHref, relatedCase, rowDatabase } from './workflow'
import { useApprovalKeys } from './useApprovalKeys'
import { BulkApproveBar } from './BulkApproveBar'
import { SAFE_ONLY, bulkKey, classGroups, isSafeTier, useBulkSelection } from './bulkApprove'

// CardFacts shows the binding facts about a card's targets (roadmap 2.3).
// Approval cards are only loaded for operators and admins (Actions gates
// the /approvals fetch on that role), so proposed facts can be decided
// here; a decision refreshes the cards, since it can change what the
// queued action may do.
function CardFacts({ card, onDecided }) {
  return (
    <FactBadges database={card.database} objects={card.targets} canDecide
      onChanged={onDecided} />
  )
}

// FocusedCard scrolls to and outlines the queued proposal a link named
// (queue=<id>, ux-01b), and the card the approval keys select once they
// have been used (ux-05a); data-selected marks the keys' target. Enter
// focuses it (ux-07b), so Tab then walks the card's own controls.
function FocusedCard({ focused, selected, keyed, wrapperRef, children }) {
  const ref = useRef(null)
  const outlined = focused || (selected && keyed)
  const setRef = el => {
    ref.current = el
    wrapperRef?.(el)
  }
  useEffect(() => {
    if (focused) ref.current?.scrollIntoView?.({ block: 'center' })
  }, [focused])
  useEffect(() => {
    if (selected && keyed) ref.current?.scrollIntoView?.({ block: 'nearest' })
  }, [selected, keyed])
  return <div ref={setRef} tabIndex={-1} className="space-y-1 rounded"
    data-testid={focused ? 'approval-card-focused' : undefined}
    data-selected={selected ? 'true' : undefined} aria-current={selected || undefined}
    style={outlined ? { outline: '2px solid var(--accent)', outlineOffset: '4px' } : undefined}>
    {children}
  </div>
}

const cardKey = card => bulkKey(card.database, card.queue_id)

// refMap keeps one ref per card key.
function useRefMap() {
  const map = useRef(new Map())
  const refFor = key => value => {
    if (value) map.current.set(key, value)
    else map.current.delete(key)
  }
  return { get: key => map.current.get(key), refFor }
}

// useCardKeys wires the approval keys to the cards' decision verbs: x marks
// a safe card for bulk approve, Enter opens the card, and a with cards
// marked opens the bulk confirm instead of the card's own (ux-07b).
function useCardKeys(cards, queueFocus, selection, bulkRef) {
  const handles = useRefMap()
  const wrappers = useRefMap()
  const toast = useToast()
  const focusedAt = cards.findIndex(card => isFocusedQueue(card, queueFocus))
  const onVerb = useCallback((verb, index) => {
    const card = cards[index]
    const key = cardKey(card)
    if (verb === 'toggle') {
      if (isSafeTier(card.risk?.tier)) selection.toggle(key)
      else toast.error(`${card.title}: ${SAFE_ONLY}`)
    } else if (verb === 'open') {
      wrappers.get(key)?.focus()
    } else if (verb === 'approve' && selection.selected.size > 0) {
      bulkRef.current?.open()
    } else {
      handles.get(key)?.[verb]?.()
    }
  }, [cards, selection, toast, bulkRef, handles, wrappers])
  const keys = useApprovalKeys(cards.length, onVerb, Math.max(focusedAt, 0))
  return { ...keys, refFor: handles.refFor, wrapperFor: wrappers.refFor }
}

// BulkCheckbox marks a safe card for bulk approve; any other is disabled
// and says why.
function BulkCheckbox({ card, checked, onToggle }) {
  const safe = isSafeTier(card.risk?.tier)
  const hint = `bulk-hint-${card.database}-${card.queue_id}`
  return <label className="inline-flex items-center gap-2 text-xs" style={{
    color: 'var(--text-secondary)' }}>
    <input type="checkbox" data-testid={`bulk-select-${card.database}-${card.queue_id}`}
      checked={checked} disabled={!safe} onChange={onToggle}
      aria-label={`Select ${card.title} for bulk approve`} aria-describedby={hint} />
    <span id={hint}>{safe ? 'Select for bulk approve' : 'Bulk approve is safe tier only'}</span>
  </label>
}

// useCardsBulk is the cards' bulk selection, groups and bar inputs.
function useCardsBulk(cards, onDecided) {
  const keys = useMemo(() => cards.map(cardKey), [cards])
  const selection = useBulkSelection(keys)
  const groups = useMemo(() => classGroups(cards.map(c => ({ key: cardKey(c),
    cls: c.action_type, database: c.database, safe: isSafeTier(c.risk?.tier) }))), [cards])
  const loadCards = async () => ({
    cards: cards.filter(c => selection.selected.has(cardKey(c))), skipped: [] })
  const labelFor = r => cards.find(c => cardKey(c) === bulkKey(r.database, r.id))?.title
  const onSettled = body => {
    selection.settle(body?.results)
    onDecided?.()
  }
  return { selection, groups, loadCards, labelFor, onSettled }
}

// PendingCardsView is the Pending Approval tab's default view: one
// approval card per waiting action, with the compact table one click away.
export function PendingCardsView({ cards, errors, onShowTable, onDecided, pending = [],
  cases = [], database, modeForDatabase, queueFocus = '' }) {
  const bulkRef = useRef(null)
  const bulk = useCardsBulk(cards, onDecided)
  const keys = useCardKeys(cards, queueFocus, bulk.selection, bulkRef)
  return (
    <div className="space-y-3" data-testid="approval-cards">
      <div className="flex items-center justify-between gap-2">
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          Review the evidence, exact SQL, risk and recovery plan. Approve and run
          requests immediate execution after current safety checks. A completed change still needs outcome verification.
        </p>
        <button data-testid="pending-view-table" onClick={onShowTable}
          className="px-2 py-1 rounded text-xs shrink-0"
          style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
          Table view
        </button>
      </div>
      <PendingErrors errors={errors} />
      {cards.length > 0 && <BulkApproveBar count={bulk.selection.selected.size}
        groups={bulk.groups} onSelectGroup={bulk.selection.add} loadCards={bulk.loadCards}
        surface="page" onSettled={bulk.onSettled} labelFor={bulk.labelFor}
        controlRef={bulkRef} />}
      {cards.length === 0 ? (
        <div data-testid="approval-cards-empty">
          <EmptyState message="Nothing needs your approval in this view. Review Proposed changes for opportunities and blocked work." />
        </div>
      ) : cards.map((card, i) => {
        const readiness = pending.find(r => rowDatabase(r, database) === card.database
          && String(r.id) === String(card.queue_id))
        const linked = relatedCase(card, cases, database)
        return <FocusedCard key={cardKey(card)} focused={isFocusedQueue(card, queueFocus)}
          selected={i === keys.selected} keyed={keys.used}
          wrapperRef={keys.wrapperFor(cardKey(card))}>
          <BulkCheckbox card={card} checked={bulk.selection.selected.has(cardKey(card))}
            onToggle={() => bulk.selection.toggle(cardKey(card))} />
          <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
            {linked ? <a href={caseHref(linked, database)} className="underline"
              style={{ color: 'var(--accent)' }}>Review issue: {linked.title}</a>
              : <>Issue link unavailable. <a href={caseHref(null, card.database)} className="underline"
                style={{ color: 'var(--accent)' }}>View issues for this database</a></>}
          </p>
          <ApprovalCard card={card} onDecided={onDecided} eligible={readiness?.eligible}
            blockedReason={readiness?.defer_reason || readiness?.blocked_reason}
            windowOnly={readiness?.policy_detail?.window_only === true}
            mode={modeForDatabase?.(card.database)} actionsRef={keys.refFor(cardKey(card))}
            facts={<CardFacts card={card} onDecided={onDecided} />} />
        </FocusedCard>
      })}
    </div>
  )
}

// ApprovalCardDetail shows a queued action's card inside its detail row.
export function ApprovalCardDetail({ action, onDecided, mode }) {
  const db = action?.database_name
    ? `?database=${encodeURIComponent(action.database_name)}` : ''
  const { data } = useAPI(action?.id ? `/api/v1/approvals/${action.id}${db}` : null, 0)
  if (!data?.card) return null
  return (
    <div data-testid="approval-card-detail">
      <ApprovalCard card={data.card} onDecided={onDecided} mode={mode}
        eligible={data.eligible ?? action.eligible}
        blockedReason={data.defer_reason || action.defer_reason || action.blocked_reason}
        windowOnly={(data.policy_detail ?? action.policy_detail)?.window_only === true}
        facts={<CardFacts card={data.card} onDecided={onDecided} />} />
    </div>
  )
}
