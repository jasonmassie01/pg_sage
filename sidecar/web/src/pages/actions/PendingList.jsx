import { useState } from 'react'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { QueryState } from '../../components/QueryState'
import { usePendingActionsRefetch } from '../../components/Layout'
import { PendingErrors } from './PendingErrors'
import { ScheduledApprovals } from './ScheduledApprovals'
import { PendingCardsView } from './ApprovalCards'
import { rowDatabase } from './workflow'
import { actionRowKey } from './actionFormat'
import { usePendingActions } from './usePendingActions'
import { PendingDetail, pendingColumns } from './PendingRow'

function PendingSkeleton() {
  return (
    <div className="space-y-2" data-testid="pending-skeleton">
      {[0, 1, 2].map(i => (
        <div key={i}
          className="h-10 rounded animate-pulse"
          style={{ background: 'var(--bg-hover)' }} />
      ))}
    </div>
  )
}

// PendingTab shows the queued actions of query (the pending list) and
// keeps them, with a strip, when a refresh fails.
export function PendingTab({ query, ...props }) {
  return <QueryState query={query} skeleton={<PendingSkeleton />}>
    {() => <>
      <ScheduledApprovals rows={props.data?.scheduled} onChanged={query.refetch} />
      <PendingList refetch={query.refetch} {...props} />
    </>}
  </QueryState>
}

// cardsFor keeps the approval cards of the queued actions and says whether
// every queued action has one; only then is the card view complete.
function cardsFor(actions, rawCards, database) {
  if (!Array.isArray(rawCards)) return { hasCards: false, cardList: [], complete: false }
  const matches = (action, card) => rowDatabase(action, database) === card.database
    && String(action.id) === String(card.queue_id)
  const cardList = rawCards.filter(card => actions.some(action => matches(action, card)))
  const complete = actions.every(action => cardList.some(card => matches(action, card)))
  return { hasCards: true, cardList, complete }
}

function ActionMessage({ msg }) {
  if (!msg) return null
  return (
    <div className="p-2 rounded text-sm" style={{
      background: msg.type === 'success' ? 'var(--green-solid)' : 'var(--red-solid)',
      color: '#fff' }}>
      {msg.text}
    </div>
  )
}

// QueueFocusMissing says a linked approval request (queue=<id>) is not
// among the waiting ones: it was decided or expired after the link was
// made (ux-05b), so the list is not silently shown as if it were focused.
// When a database could not be read (errors), the request may still wait
// there, so the notice does not claim it was decided.
function QueueFocusMissing({ id, actions, errors }) {
  if (!id || actions.some(action => String(action.id) === id)) return null
  const partial = Array.isArray(errors) && errors.length > 0
  return <p role="status" data-testid="approval-queue-missing" className="text-sm"
    style={{ color: 'var(--yellow)' }}>
    {partial
      ? `Request #${id} is not in this list: it was decided or expired, or its `
        + 'database could not be read.'
      : `Request #${id} is no longer waiting on you: it was decided or expired.`}
    {' '}History lists what was run.
  </p>
}

// PendingList leads with the approval cards when every queued action has one
// and otherwise shows the table, where each row is approved or rejected.
export function PendingList({ data, refetch, cards, cases, database, modeForDatabase,
  queueFocus = '' }) {
  const [view, setView] = useState('cards')
  const refetchPendingCount = usePendingActionsRefetch()
  const decision = usePendingActions(refetch)
  const actions = data?.pending || []
  const { hasCards, cardList, complete } = cardsFor(actions, cards?.data?.cards, database)
  const refreshAll = () => {
    refetch()
    cards?.refetch?.()
    refetchPendingCount()
  }
  const missing = <QueueFocusMissing id={queueFocus} actions={actions}
    errors={data?.errors} />
  if (complete && view === 'cards') {
    return <>
      {missing}
      <PendingCardsView cards={cardList} errors={cards.data.errors}
        onShowTable={() => setView('table')} onDecided={refreshAll}
        pending={actions} cases={cases} database={database} modeForDatabase={modeForDatabase}
        queueFocus={queueFocus} />
    </>
  }
  return (
    <div className="space-y-3">
      {missing}
      {hasCards && (
        <button data-testid="pending-view-cards" onClick={() => setView('cards')}
          className="px-2 py-1 rounded text-xs"
          style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
          Card view
        </button>
      )}
      <p data-testid="pending-help-text" className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        Review the evidence, exact SQL and recovery plan before deciding.
        Approve and run requests immediate execution after current safety checks;
        it does not establish that the change helped.
      </p>
      <ActionMessage msg={decision.actionMsg} />
      <PendingErrors errors={data?.errors} />
      {actions.length === 0 ? (
        <EmptyState message={'Nothing needs your approval in this view. '
          + 'Review Proposed changes for opportunities and blocked work.'} />
      ) : <DataTable data-testid="pending-actions-table"
        columns={pendingColumns({ actions, database, modeForDatabase, decision })}
        rows={actions} expandable rowKey={actionRowKey}
        renderExpanded={row => <PendingDetail row={row} cases={cases} database={database}
          modeForDatabase={modeForDatabase} onDecided={refreshAll} />} />}
    </div>
  )
}
