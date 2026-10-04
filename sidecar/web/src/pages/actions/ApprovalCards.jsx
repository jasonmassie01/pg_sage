import { useAPI } from '../../hooks/useAPI'
import { EmptyState } from '../../components/EmptyState'
import { ApprovalCard } from './ApprovalCard'
import { PendingErrors } from './PendingErrors'

// PendingCardsView is the Pending Approval tab's default view: one
// approval card per waiting action, with the compact table one click away.
export function PendingCardsView({ cards, errors, onShowTable, onDecided }) {
  return (
    <div className="space-y-3" data-testid="approval-cards">
      <div className="flex items-center justify-between gap-2">
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          Each card says why the action needs you, the evidence behind it, the exact
          SQL and how to undo it. Approve runs it now through the same policy checks.
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
          <EmptyState message="Nothing is waiting for your approval." />
        </div>
      ) : cards.map(card => (
        <ApprovalCard key={`${card.database}:${card.queue_id}`} card={card}
          onDecided={onDecided} />
      ))}
    </div>
  )
}

// ApprovalCardDetail shows a queued action's card inside its detail row.
export function ApprovalCardDetail({ action, onDecided }) {
  const db = action?.database_name
    ? `?database=${encodeURIComponent(action.database_name)}` : ''
  const { data } = useAPI(action?.id ? `/api/v1/approvals/${action.id}${db}` : null, 0)
  if (!data?.card) return null
  return (
    <div data-testid="approval-card-detail">
      <ApprovalCard card={data.card} onDecided={onDecided} />
    </div>
  )
}
