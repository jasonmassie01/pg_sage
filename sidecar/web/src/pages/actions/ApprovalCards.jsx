import { useCallback, useEffect, useRef } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { isFocusedQueue } from './actionFocus'
import { EmptyState } from '../../components/EmptyState'
import { FactBadges } from '../../components/FactBadges'
import { ApprovalCard } from './ApprovalCard'
import { PendingErrors } from './PendingErrors'
import { caseHref, relatedCase, rowDatabase } from './workflow'
import { useApprovalKeys } from './useApprovalKeys'

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
// have been used (ux-05a); data-selected marks the keys' target.
function FocusedCard({ focused, selected, keyed, children }) {
  const ref = useRef(null)
  const outlined = focused || (selected && keyed)
  useEffect(() => {
    if (focused) ref.current?.scrollIntoView?.({ block: 'center' })
  }, [focused])
  useEffect(() => {
    if (selected && keyed) ref.current?.scrollIntoView?.({ block: 'nearest' })
  }, [selected, keyed])
  return <div ref={ref} className="space-y-1 rounded"
    data-testid={focused ? 'approval-card-focused' : undefined}
    data-selected={selected ? 'true' : undefined} aria-current={selected || undefined}
    style={outlined ? { outline: '2px solid var(--accent)', outlineOffset: '4px' } : undefined}>
    {children}
  </div>
}

const cardKey = card => `${card.database}:${card.queue_id}`

// useCardKeys wires the approval keys to the cards' decision verbs.
function useCardKeys(cards, queueFocus) {
  const handles = useRef(new Map())
  const focusedAt = cards.findIndex(card => isFocusedQueue(card, queueFocus))
  const onVerb = useCallback((verb, index) => {
    handles.current.get(cardKey(cards[index]))?.[verb]?.()
  }, [cards])
  const keys = useApprovalKeys(cards.length, onVerb, Math.max(focusedAt, 0))
  const refFor = key => handle => {
    if (handle) handles.current.set(key, handle)
    else handles.current.delete(key)
  }
  return { ...keys, refFor }
}

// PendingCardsView is the Pending Approval tab's default view: one
// approval card per waiting action, with the compact table one click away.
export function PendingCardsView({ cards, errors, onShowTable, onDecided, pending = [],
  cases = [], database, modeForDatabase, queueFocus = '' }) {
  const keys = useCardKeys(cards, queueFocus)
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
      {cards.length === 0 ? (
        <div data-testid="approval-cards-empty">
          <EmptyState message="Nothing needs your approval in this view. Review Proposed changes for opportunities and blocked work." />
        </div>
      ) : cards.map((card, i) => {
        const readiness = pending.find(r => rowDatabase(r, database) === card.database
          && String(r.id) === String(card.queue_id))
        const linked = relatedCase(card, cases, database)
        return <FocusedCard key={cardKey(card)} focused={isFocusedQueue(card, queueFocus)}
          selected={i === keys.selected} keyed={keys.used}>
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
