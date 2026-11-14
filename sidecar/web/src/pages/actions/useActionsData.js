import { useAPI } from '../../hooks/useAPI'
import { useLiveRefetch } from '../../hooks/useLiveEvents'
import { useTimeRange } from '../../context/TimeRangeContext'
import { isQueuedRow } from './ledger'
import { caseID, proposedChanges, relatedCase, rowDatabase } from './workflow'
import { useExecutedActions } from './useExecutedActions'

// useActionsData loads everything the Actions page shows and derives the
// per-tab rows and counts for the case in scope (split out of Actions.jsx
// by dashboard-31; the requests and polling are unchanged).

const ACTIVE_STATES = ['approved', 'applying', 'applied', 'verifying']
const CURRENT_STATES = ['proposed', ...ACTIVE_STATES]

function useActionQueries(database, canReview, single, dbParam) {
  const pending = useAPI(canReview ? `/api/v1/actions/pending${dbParam}` : null)
  const cards = useAPI(canReview ? `/api/v1/approvals${dbParam}` : null)
  const caseData = useAPI(`/api/v1/cases${dbParam}`)
  const recommendations = useAPI(single
    ? `/api/v1/recommendations?database=${encodeURIComponent(database)}` : null)
  const managed = useAPI(`/api/v1/managed-changes${dbParam}`)
  const scope = { database }
  useLiveRefetch(['actions'], canReview ? pending.refetch : null, scope)
  useLiveRefetch(['actions'], canReview ? cards.refetch : null, scope)
  useLiveRefetch(['actions'], recommendations.refetch, scope)
  return { pending, cards, caseData, recommendations, managed }
}

// ledgerRecs places stored recommendations without a matching change record
// in In progress or History; a focused action shows only its own record.
function ledgerRecs({ recs, cases, database, actionFocus, progressRows }) {
  if (actionFocus) return { progressRecs: [], historyRecs: [] }
  const linked = r => ({ ...r, linkedCase: relatedCase(r, cases, database) })
  const recorded = r => progressRows.some(a => rowDatabase(a, database) === rowDatabase(r, database)
    && !isQueuedRow(a) && String(a.id) === String(r.action_log_id))
  return {
    progressRecs: recs.filter(r => ACTIVE_STATES.includes(r.state)).map(linked)
      .filter(r => !r.action_log_id || !recorded(r)),
    historyRecs: recs.filter(r => !CURRENT_STATES.includes(r.state) && !r.action_log_id)
      .map(linked),
  }
}

function loadState(loading, error) {
  if (loading) return '…'
  return error ? '?' : null
}

// tabCounts is the loaded-item count per tab: … while loading, ? on an
// error, and + when more may be available.
function tabCounts({ q, ledger, single, proposals, recs, pendingRows, managedCount, more }) {
  const { pending, managed, recommendations, caseData } = q
  const recsLoading = single && recommendations.loading
  const actions = ledger.query
  return {
    recommendations: loadState(caseData.loading || recsLoading,
      caseData.error || recommendations.error)
      ?? `${proposals.length}${recs.length >= 100 ? '+' : ''}`,
    pending: loadState(pending.loading || managed.loading,
      pending.error || managed.error || pending.data?.errors?.length)
      ?? String(pendingRows.length + managedCount('pending')),
    progress: loadState(actions.loading || recsLoading || managed.loading,
      actions.error || recommendations.error || managed.error)
      ?? `${ledger.progressRows.length + ledger.progressRecs.length
        + managedCount('approved')}${more}`,
    executed: loadState(actions.loading || recsLoading, actions.error || recommendations.error)
      ?? `${ledger.historyRows.length + ledger.historyRecs.length}${more}`,
  }
}

function scopedResponses(q, scoped) {
  const { pending, cards, managed } = q
  const databases = (managed.data?.databases || []).map(d => ({ ...d,
    proposals: (d.proposals || []).filter(r => scoped({ ...r, database: d.database })) }))
  return {
    managedDatabases: databases,
    managedCount: state => databases.reduce((n, d) =>
      n + d.proposals.filter(r => r.status === state).length, 0),
    pendingData: { ...pending.data, pending: (pending.data?.pending || []).filter(scoped),
      scheduled: (pending.data?.scheduled || []).filter(scoped) },
    cards: { ...cards, data: { ...cards.data, ...(Array.isArray(cards.data?.cards)
      ? { cards: cards.data.cards.filter(scoped) } : {}) } },
  }
}

export function useActionsData({ database, canReview, caseScope, actionFocus }) {
  const range = useTimeRange()
  const single = database && database !== 'all'
  const dbParam = single ? `?database=${encodeURIComponent(database)}` : ''
  const q = useActionQueries(database, canReview, single, dbParam)
  const cases = q.caseData.data?.cases || []
  const scoped = row => !caseScope
    || String(caseID(relatedCase(row, cases, database))) === caseScope
  const executed = useExecutedActions({ database, range, dbParam, scoped, actionFocus })
  const recs = (q.recommendations.data?.recommendations || []).filter(scoped)
  const scopedCases = caseScope ? cases.filter(c => String(caseID(c)) === caseScope) : cases
  const s = scopedResponses(q, scoped)
  const pendingFor = row => s.pendingData.pending.find(a =>
    rowDatabase(a, database) === rowDatabase(row, database)
    && String(a.recommendation_id) === String(row.id))
  const proposals = proposedChanges(recs, scopedCases, database)
    .map(r => ({ ...r, pendingAction: pendingFor(r) }))
  const ledger = { ...executed, ...ledgerRecs({ recs, cases, database, actionFocus,
    progressRows: executed.progressRows }) }
  const counts = tabCounts({ q, ledger, single, proposals, recs,
    pendingRows: s.pendingData.pending, managedCount: s.managedCount,
    more: executed.paging.hasMore ? '+' : '' })
  const refreshAll = () => {
    executed.query.refetch(); q.pending.refetch(); q.cards.refetch()
    q.recommendations.refetch(); q.caseData.refetch()
  }
  return { range, single, dbParam, q, cases, proposals, ledger, counts, refreshAll,
    selectedCase: caseScope ? cases.find(c => String(caseID(c)) === caseScope) : null,
    managedDatabases: s.managedDatabases, pendingData: s.pendingData, cards: s.cards }
}
