import { useAPI } from '../../hooks/useAPI'
import { EmptyState } from '../../components/EmptyState'
import { FactBadges } from '../../components/FactBadges'
import { ApprovalCard } from './ApprovalCard'
import { PendingErrors } from './PendingErrors'
import { caseHref, relatedCase, rowDatabase } from './workflow'

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

// PendingCardsView is the Pending Approval tab's default view: one
// approval card per waiting action, with the compact table one click away.
export function PendingCardsView({ cards, errors, onShowTable, onDecided, pending = [], cases = [], database, modeForDatabase }) {
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
      ) : cards.map(card => {
        const readiness = pending.find(r => rowDatabase(r, database) === card.database
          && String(r.id) === String(card.queue_id))
        const linked = relatedCase(card, cases, database)
        return <div key={`${card.database}:${card.queue_id}`} className="space-y-1">
          <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
            {linked ? <a href={caseHref(linked, database)} className="underline"
              style={{ color: 'var(--accent)' }}>Review case: {linked.title}</a>
              : <>Case relation unavailable. <a href={caseHref(null, card.database)} className="underline"
                style={{ color: 'var(--accent)' }}>View Cases for this database</a></>}
          </p>
          <ApprovalCard card={card} onDecided={onDecided} eligible={readiness?.eligible}
            blockedReason={readiness?.defer_reason || readiness?.blocked_reason}
            mode={modeForDatabase?.(card.database)}
            facts={<CardFacts card={card} onDecided={onDecided} />} />
        </div>
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
        facts={<CardFacts card={data.card} onDecided={onDecided} />} />
    </div>
  )
}
