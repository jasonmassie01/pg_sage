import { useEffect, useState } from 'react'
import { useAPI, withTimeRange } from '../hooks/useAPI'
import { useTimeRange } from '../context/TimeRangeContext'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { LiveTimeAgo } from '../components/TimeAgo'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'
import { SkeletonRow } from '../components/Skeleton'
import { usePendingActionsRefetch } from '../components/Layout'
import { useToast } from '../components/Toast'
import { useLiveRefetch } from '../hooks/useLiveEvents'
import { formatTotal, useCursorPages } from '../lib/listPaging'
import { LoadMore } from '../components/LoadMore'
import { canRollBackRow, isQueuedRow, queuedLabels } from './actions/ledger'
import { PendingErrors } from './actions/PendingErrors'
import { IndexAdmissionPanel } from '../components/IndexAdmissionPanel'
import { VerificationOutcome } from '../components/VerificationOutcome'
import { verdictLabel } from '../lib/verificationOutcome'
import { RecommendationsTab } from './actions/RecommendationsTab'
import { revisionPin } from './actions/recommendation'
import { ApprovalCardDetail, PendingCardsView } from './actions/ApprovalCards'
import { ManagedChanges } from './actions/ManagedChanges'
import { approvalOutcomeMessage } from './actions/ApprovalCard'
import { actionLabel, awaitingReview, blockerText, caseHref, caseID, inProgress, proposedChanges, relatedCase, riskLabel, rowDatabase } from './actions/workflow'

function actionStatus(row) {
  return row.status || row.action_status || row.outcome || 'unknown'
}

function actionRisk(row) {
  return row.risk_tier || row.action_risk || row.risk || 'unknown'
}

function verificationStatus(row) {
  if (row.verification_outcome) return verdictLabel(row.verification_outcome.verdict)
  return row.verification_status || 'not_started'
}


function lifecycleStatus(row) {
  return row.lifecycle_state || row.status || 'ready'
}

function rollbackClassLabel(value) {
  switch (value) {
  case 'no_rollback_needed':
    return 'Rollback: not needed'
  case 'reversible':
    return 'Rollback: reversible'
  case 'forward_fix_only':
    return 'Rollback: forward fix only'
  default:
    return value ? `Rollback: ${value}` : null
  }
}

function formatActionTime(value) {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return null
  return date.toLocaleString()
}

function actionCaseScope() {
  const query = (window.location.hash || '').split('?')[1] || ''
  return new URLSearchParams(query).get('case') || ''
}

export function Actions({ database, user, fleetData }) {
  const [tab, setTab] = useState('recommendations')
  const [caseScope, setCaseScope] = useState(actionCaseScope)
  useEffect(() => {
    const changed = () => setCaseScope(actionCaseScope())
    window.addEventListener('hashchange', changed)
    return () => window.removeEventListener('hashchange', changed)
  }, [])
  const range = useTimeRange()
  const canReview = user?.role === 'admin' || user?.role === 'operator'
  const activeTab = canReview || tab !== 'pending' ? tab : 'recommendations'
  const single = database && database !== 'all'
  const dbParam = single ? `?database=${encodeURIComponent(database)}` : ''
  const actionsURL = withTimeRange(`/api/v1/actions${dbParam}`, range)
  const { data, loading, error, refetch } = useAPI(actionsURL)
  const paging = useCursorPages(data, actionsURL, 'actions')
  const pending = useAPI(canReview ? `/api/v1/actions/pending${dbParam}` : null)
  const cards = useAPI(canReview ? `/api/v1/approvals${dbParam}` : null)
  const caseData = useAPI(`/api/v1/cases${dbParam}`)
  const recommendations = useAPI(single
    ? `/api/v1/recommendations?database=${encodeURIComponent(database)}` : null)
  const managed = useAPI(`/api/v1/managed-changes${dbParam}`)
  useLiveRefetch(['actions'], refetch)
  useLiveRefetch(['actions'], canReview ? pending.refetch : null)
  useLiveRefetch(['actions'], canReview ? cards.refetch : null)
  useLiveRefetch(['actions'], recommendations.refetch)

  const cases = caseData.data?.cases || []
  const selectedCase = caseScope ? cases.find(c => String(caseID(c)) === caseScope) : null
  const scoped = row => !caseScope || String(caseID(relatedCase(row, cases, database))) === caseScope
  const scopedCases = caseScope ? cases.filter(c => String(caseID(c)) === caseScope) : cases
  const recs = (recommendations.data?.recommendations || []).filter(scoped)
  const pendingRows = (pending.data?.pending || []).filter(scoped)
  const ledger = paging.rows.filter(scoped)
  const modeForDatabase = db => fleetData?.databases?.find(d => d.name === db)?.status?.trust_level || null
  const pendingFor = row => pendingRows.find(a => rowDatabase(a, database) === rowDatabase(row, database)
    && String(a.recommendation_id) === String(row.id))
  const proposals = proposedChanges(recs, scopedCases, database)
    .map(r => ({ ...r, pendingAction: pendingFor(r) }))
  const activeRecs = recs.filter(r => ['approved', 'applying', 'applied', 'verifying'].includes(r.state))
    .map(r => ({ ...r, linkedCase: relatedCase(r, cases, database) }))
  const progressRows = ledger.filter(inProgress)
  const progressRecs = activeRecs.filter(r => !r.action_log_id || !progressRows.some(a =>
    rowDatabase(a, database) === rowDatabase(r, database) && !isQueuedRow(a)
    && String(a.id) === String(r.action_log_id)))
  const historyRows = ledger.filter(r => !inProgress(r) && !awaitingReview(r))
  const historyRecs = recs.filter(r => !['proposed', 'approved', 'applying', 'applied', 'verifying'].includes(r.state)
    && !r.action_log_id).map(r => ({ ...r, linkedCase: relatedCase(r, cases, database) }))
  const scopedManaged = (managed.data?.databases || []).map(d => ({ ...d,
    proposals: (d.proposals || []).filter(r => scoped({ ...r, database: d.database })) }))
  const managedCount = state => scopedManaged.reduce((n, d) => n + d.proposals.filter(r => r.status === state).length, 0)
  const proposalUnknown = caseData.loading || (single && recommendations.loading)
  const proposalError = caseData.error || recommendations.error
  const counts = {
    recommendations: proposalUnknown ? '…' : proposalError ? '?' : `${proposals.length}${recs.length >= 100 ? '+' : ''}`,
    pending: pending.loading || managed.loading ? '…' : pending.error || managed.error || pending.data?.errors?.length ? '?'
      : String(pendingRows.length + managedCount('pending')),
    progress: loading || (single && recommendations.loading) || managed.loading ? '…'
      : error || recommendations.error || managed.error ? '?'
        : `${progressRows.length + progressRecs.length + managedCount('approved')}${paging.hasMore ? '+' : ''}`,
    executed: loading || (single && recommendations.loading) ? '…' : error || recommendations.error ? '?'
      : `${historyRows.length + historyRecs.length}${paging.hasMore ? '+' : ''}`,
  }
  const scopedPending = { ...pending.data, pending: pendingRows }
  const scopedCards = { ...cards, data: { ...cards.data,
    ...(Array.isArray(cards.data?.cards) ? { cards: cards.data.cards.filter(scoped) } : {}) } }
  const refreshAll = () => { refetch(); pending.refetch(); cards.refetch(); recommendations.refetch(); caseData.refetch() }

  return (
    <div className="space-y-4" data-testid="actions-page">
      <p className="text-sm" data-testid="actions-page-description"
        style={{ color: 'var(--text-secondary)' }}>
        Review proposed database changes, decide which requests to approve, and track results.
        Cases explain the evidence behind a change. Approval, execution, and a verified improvement are separate steps.
      </p>
      {caseScope && <div className="rounded border p-3 text-sm space-y-1" data-testid="actions-case-context"
        style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
        <p>{selectedCase ? `Changes for case: ${selectedCase.title}`
          : caseData.loading ? 'Loading the linked case…'
            : caseData.error ? 'The linked case could not be loaded. Its related changes cannot be confirmed.'
              : 'The linked case is unavailable in this database view.'}</p>
        {caseData.error && <ErrorBanner message={caseData.error} onRetry={caseData.refetch} />}
        {selectedCase && <a href={caseHref(selectedCase, database)} className="underline"
          style={{ color: 'var(--accent)' }}>Review case evidence</a>}
        <a href={`#/actions${single ? `?database=${encodeURIComponent(database)}` : ''}`}
          className="ml-3 underline" style={{ color: 'var(--accent)' }} onClick={() => setCaseScope('')}>
          Show all changes</a>
      </div>}
      <p className="text-xs" data-testid="actions-scope" style={{ color: 'var(--text-secondary)' }}>
        {single ? database : 'All selected databases'} · Proposals and requests show current work.
        {' '}In progress and History use {range?.range?.label?.toLowerCase() || 'the selected time range'} for change records.
        {' '}Counts cover loaded items; + means more may be available.
        {!single && ' Stored recommendations require a single database; case proposals are shown across the fleet.'}
        {!canReview && ' Your role can inspect changes; approval requests are available to operators and admins.'}
      </p>
      {single && modeForDatabase(database) === 'observation' && <p className="text-sm" data-testid="actions-observation"
        style={{ color: 'var(--yellow)' }}>
        Database changes are disabled in Observation mode. Review Cases and proposed changes; no database changes will run in this mode.
      </p>}
      <TabBar tab={activeTab} setTab={setTab} counts={counts} canReview={canReview} />
      {activeTab === 'recommendations' && <RecommendationsTab database={database} items={proposals}
        loading={proposalUnknown} error={proposalError} refetch={() => { caseData.refetch(); recommendations.refetch() }}
        modeForDatabase={modeForDatabase} onReview={canReview ? () => setTab('pending') : null} />}
      {activeTab === 'pending' && <>
        <ManagedChanges database={database} canDecide={canReview} response={{ ...managed,
          data: { ...managed.data, databases: scopedManaged } }} statuses={['pending']} />
        <PendingTab data={scopedPending} loading={pending.loading} error={pending.error}
          refetch={refreshAll} cards={scopedCards} cases={cases} database={database} modeForDatabase={modeForDatabase} />
      </>}
      {activeTab === 'progress' && <>
        {recommendations.error && <ErrorBanner message={`Recommendation progress could not be loaded: ${recommendations.error}`} onRetry={recommendations.refetch} />}
        <ManagedChanges database={database} canDecide={false} response={{ ...managed,
          data: { ...managed.data, databases: scopedManaged } }} statuses={['approved']} />
        {progressRecs.length > 0 && <RecommendationsTab database={database} items={progressRecs}
          modeForDatabase={modeForDatabase} view="progress" />}
        <ExecutedTab data={data} paging={{ ...paging, rows: progressRows }} loading={loading}
          error={error} refetch={refreshAll} user={user} view="progress" cases={cases} database={database} />
      </>}
      {activeTab === 'executed' && <>
        {recommendations.error && <ErrorBanner message={`Recommendation outcomes could not be loaded: ${recommendations.error}`} onRetry={recommendations.refetch} />}
        {historyRecs.length > 0 && <RecommendationsTab database={database} items={historyRecs}
          modeForDatabase={modeForDatabase} view="history" />}
        <ExecutedTab data={data} paging={{ ...paging, rows: historyRows }} loading={loading}
          error={error} refetch={refreshAll} user={user} view="history" cases={cases} database={database} />
      </>}
      <details className="rounded border p-3 text-sm" data-testid="actions-safety-details"
        style={{ borderColor: 'var(--border)' }}>
        <summary className="cursor-pointer">Index-build safety conditions</summary>
        <p className="mt-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
          Disk-load history is one safety check. Completing a baseline does not enable execution in Observation mode.
          Current mode, permissions, supported actions, approval requirements and other safety checks still apply.
        </p>
        <IndexAdmissionPanel database={database} />
      </details>
    </div>
  )
}

function TabBar({ tab, setTab, counts, canReview }) {
  const tabs = [{ key: 'recommendations', label: 'Proposed changes' }]
  if (canReview) tabs.push({ key: 'pending', label: 'Needs your review' })
  tabs.push({ key: 'progress', label: 'In progress' }, { key: 'executed', label: 'History' })
  return <div className="flex flex-wrap gap-2" role="tablist" aria-label="Change tasks">
    {tabs.map(t => <button key={t.key} type="button" role="tab" aria-selected={tab === t.key}
      onClick={() => setTab(t.key)} data-testid={`actions-tab-${t.key}`}
      className="px-3 py-1.5 rounded text-sm" style={{
        background: tab === t.key ? 'var(--accent)' : 'var(--bg-card)',
        color: tab === t.key ? '#fff' : 'var(--text-secondary)', border: '1px solid var(--border)' }}>
      {t.label} <span className="ml-1.5" aria-label={`${counts[t.key]} loaded items`}>({counts[t.key]})</span>
    </button>)}
  </div>
}

function ExecutedTab({ data, paging, loading, error, refetch, user, view, cases, database }) {
  const toast = useToast()
  const canRollback = user?.role === 'admin' || user?.role === 'operator'
  if (loading) {
    return (
      <div className="space-y-2"
        data-testid="executed-actions-loading">
        {Array.from({ length: 6 }).map((_, i) => (
          <SkeletonRow key={i} cols={4} />
        ))}
      </div>
    )
  }
  if (error) return <ErrorBanner message={error}
    onRetry={refetch} />

  const actions = paging.rows

  const outcomeStyle = outcome => {
    switch (outcome) {
    case 'success':
      return { bg: 'rgba(34,197,94,0.15)', color: 'var(--green)',
        label: 'Success' }
    case 'failed':
      return { bg: 'rgba(239,68,68,0.15)', color: 'var(--red)',
        label: 'Failed' }
    case 'expired':
      return { bg: 'rgba(107,114,128,0.15)',
        color: 'var(--text-secondary)', label: 'Expired' }
    case 'rejected':
      return { bg: 'rgba(107,114,128,0.15)',
        color: 'var(--text-secondary)', label: 'Rejected' }
    case 'rolled_back':
      return { bg: 'rgba(245,158,11,0.15)',
        color: 'var(--yellow)', label: 'Rolled Back' }
    case 'pending':
      return { bg: 'rgba(59,130,246,0.15)',
        color: 'var(--blue, #3b82f6)', label: 'Monitoring' }
    default:
      return { bg: 'rgba(107,114,128,0.15)',
        color: 'var(--text-secondary)',
        label: outcome || 'Unknown' }
    }
  }

  const actionSummary = r => {
    if (isQueuedRow(r)) return `Proposed: ${actionLabel(r.action_type)}`
    const t = (r.action_type || '').toLowerCase()
    const sql = r.sql_executed || ''
    const target = sql.match(
      /(?:public\.)?([\w.]+)\s*[;(]/i)?.[1] || ''
    if (r.outcome === 'failed' && r.rollback_reason) {
      const short = r.rollback_reason.length > 80
        ? r.rollback_reason.slice(0, 80) + '...'
        : r.rollback_reason
      return <span style={{ color: 'var(--red)' }}>
        {short}
      </span>
    }
    if ((r.outcome === 'expired' || r.outcome === 'rejected') &&
      r.rollback_reason) {
      return r.rollback_reason
    }
    if (t === 'drop_index') {
      return `Dropped index${target ? ' ' + target : ''}`
    }
    if (t === 'create_index') {
      return `Created index${target ? ' on ' + target : ''}`
    }
    if (t === 'vacuum') {
      return `Vacuumed${target ? ' ' + target : ' table'}`
    }
    if (t === 'analyze') {
      return `Updated statistics${target
        ? ' for ' + target : ''}`
    }
    if (t === 'reindex') {
      return `Reindexed${target ? ' ' + target : ''}`
    }
    if (t === 'alter') {
      return `Altered${target ? ' ' + target : ' object'}`
    }
    const raw = r.action_type || ''
    return raw.charAt(0).toUpperCase() + raw.slice(1)
  }

  const columns = [
    {
      key: 'action_type', label: 'Type',
      render: r => {
        const t = (r.action_type || '').replace(/_/g, ' ')
        return t.charAt(0).toUpperCase() + t.slice(1)
      },
    },
    { key: 'summary', label: 'Summary', render: actionSummary },
    {
      key: 'outcome', label: 'Outcome',
      render: r => {
        const s = outcomeStyle(actionStatus(r))
        if (isQueuedRow(r)) {
          s.label = queuedLabels[actionStatus(r)] || s.label
        }
        return (
          <span className="px-2 py-0.5 rounded-full text-xs
            font-medium inline-block"
            style={{ background: s.bg, color: s.color }}>
            {s.label}
          </span>
        )
      },
    },
    {
      key: 'action_risk', label: 'Risk',
      render: r => {
        const risk = actionRisk(r)
        const c = { safe: 'var(--green)', moderate: '#b58900',
          high: 'var(--red)', high_risk: 'var(--red)' }[risk] || 'var(--text-secondary)'
        return (
          <span className="px-2 py-0.5 rounded-full text-xs font-medium
            inline-block"
            title="risk tier; executed rows are only logged here, queued rows await approval"
            style={{ border: `1px solid ${c}`, color: c }}>
            {riskLabel(risk)}
          </span>
        )
      },
    },
    ...(actions.some(r => Number(r.attempts) > 1)
      ? [{
        key: 'attempts', label: 'Attempts',
        render: r => {
          const n = Number(r.attempts) || 1
          return n > 1
            ? <span title="times pg_sage attempted this action"
                style={{ color: 'var(--amber, #b58900)' }}>{n}×</span>
            : <span style={{ color: 'var(--text-secondary)' }}>1</span>
        },
      }] : []),
    ...(actions.some(r => r.verification_status || r.verification_outcome)
      ? [{
        key: 'verification_status', label: 'Verification',
        render: r => verificationStatus(r),
      }] : []),
    ...(actions.some(r => r.database_name)
      ? [{ key: 'database_name', label: 'Database' }] : []),
    {
      key: 'executed_at', label: 'When',
      render: r => <LiveTimeAgo timestamp={r.executed_at} />,
    },
  ]

  async function handleRollback(row) {
    if (!window.confirm('Run the stored rollback SQL for this action?')) {
      return
    }
    const dbParam = row.database_name
      ? `?database=${encodeURIComponent(row.database_name)}` : ''
    try {
      const res = await fetch(`/api/v1/actions/${row.id}/rollback${dbParam}`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          reason: 'manual rollback from actions UI',
          record_kind: row.record_kind,
        }),
      })
      let json = {}
      try { json = await res.json() } catch { /* non-JSON error body */ }
      if (!res.ok || !json.ok) {
        toast.error(json.error || `Rollback failed (${res.status})`)
        return
      }
      toast.success(`Action ${row.id} rolled back`)
      refetch()
    } catch (err) {
      // A destructive rollback must never fail silently — a transport
      // error here leaves the operator unsure whether the SQL ran.
      toast.error(err.message || 'Rollback request failed')
    }
  }

  if (actions.length === 0) {
    return <div className="space-y-2">
      <EmptyState message={view === 'progress'
        ? 'No change records are currently running or awaiting verification in the selected time range.'
        : 'No completed change records are loaded for the selected time range.'} />
      <LoadMore paging={paging} testId="actions-load-more" />
    </div>
  }

  return (
    <div className="space-y-3">
      <DataTable data-testid="executed-actions-table"
        columns={columns} rows={actions} expandable rowKey={actionRowKey}
        renderExpanded={row => (
          <div className="space-y-3">
            {row.outcome === 'failed' && row.rollback_reason && (
              <div className="p-3 rounded text-sm"
                style={{
                  background: 'rgba(239,68,68,0.1)',
                  border: '1px solid rgba(239,68,68,0.3)',
                }}>
                <div className="text-xs font-medium mb-1"
                  style={{ color: 'var(--red)' }}>
                  Error Details
                </div>
                <code className="text-xs" style={{
                  color: 'var(--text-primary)',
                  wordBreak: 'break-all',
                }}>
                  {row.rollback_reason}
                </code>
              </div>
            )}
            {row.outcome === 'rolled_back'
              && row.rollback_reason && (
              <div className="p-3 rounded text-sm"
                style={{
                  background: 'rgba(245,158,11,0.1)',
                  border: '1px solid rgba(245,158,11,0.3)',
                }}>
                <div className="text-xs font-medium mb-1"
                  style={{ color: 'var(--yellow)' }}>
                  Rollback Reason
                </div>
                <span className="text-xs" style={{
                  color: 'var(--text-primary)',
                }}>
                  {row.rollback_reason}
                </span>
              </div>
            )}
            <div>
              <div className="text-xs font-medium mb-1"
                style={{ color: 'var(--text-secondary)' }}>
                {isQueuedRow(row) || row.outcome === 'expired'
                  || row.outcome === 'rejected'
                  ? 'Proposed SQL' : 'SQL Executed'}
              </div>
              <SQLBlock sql={row.sql_executed} />
            </div>
            <CaseRelation row={row} cases={cases} database={database} />
            <VerificationOutcome outcome={row.verification_outcome} />
            {row.rollback_sql && (
              <div>
                <div className="text-xs font-medium mb-1"
                  style={{ color: 'var(--text-secondary)' }}>
                  Rollback SQL
                </div>
                <SQLBlock sql={row.rollback_sql} />
                {canRollback && canRollBackRow(row) && (
                  <button
                    type="button"
                    data-testid="rollback-action-button"
                    onClick={() => handleRollback(row)}
                    className="mt-2 px-2 py-1 rounded text-xs"
                    style={{
                      background: 'var(--yellow)',
                      color: '#111827',
                    }}>
                    Roll Back Action
                  </button>
                )}
              </div>
            )}
            <div className="flex gap-4 text-xs" style={{
              color: 'var(--text-secondary)',
            }}>
              {row.finding_id && (
                <span>Finding #{row.finding_id}</span>
              )}
              {row.database_name && (
                <span>Database: {row.database_name}</span>
              )}
              {row.measured_at && (
                <span>Verified: <LiveTimeAgo
                  timestamp={row.measured_at} /></span>
              )}
            </div>
          </div>
        )}
      />
      <LoadMore paging={paging} testId="actions-load-more" />
      <div className="text-xs" data-testid="executed-actions-count"
        style={{ color: 'var(--text-secondary)' }}>
        {actions.length} loaded {view === 'progress' ? 'in-progress' : 'history'} records
        {' · '}{formatTotal(data)} total change records across all cases in the selected time range
      </div>
    </div>
  )
}

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

function actionRowKey(row) {
  // ledger_key (log:N / queue:N) is unique across both id spaces (G9-B14).
  return JSON.stringify([row.database_name || '', row.ledger_key || row.id])
}

function PendingTab({
  data, loading, error, refetch, cards, cases, database, modeForDatabase,
}) {
  const [view, setView] = useState('cards')
  const [rejectId, setRejectId] = useState(null)
  const [rejectReason, setRejectReason] = useState('')
  const [actionMsg, setActionMsg] = useState(null)
  const refetchPendingCount = usePendingActionsRefetch()
  const toast = useToast()

  if (loading) return <PendingSkeleton />
  if (error) return <ErrorBanner message={error}
    onRetry={refetch} />

  const actions = data?.pending || []
  const rawCards = cards?.data?.cards
  const hasCards = Array.isArray(rawCards)
  const cardList = hasCards ? rawCards.filter(card => actions.some(action =>
    rowDatabase(action, database) === card.database && String(action.id) === String(card.queue_id))) : []
  const completeCards = hasCards && actions.every(action => cardList.some(card =>
    rowDatabase(action, database) === card.database && String(action.id) === String(card.queue_id)))
  const refreshAll = () => {
    refetch()
    cards?.refetch?.()
    refetchPendingCount()
  }
  if (completeCards && view === 'cards') {
    return <PendingCardsView cards={cardList} errors={cards.data.errors}
      onShowTable={() => setView('table')} onDecided={refreshAll}
      pending={actions} cases={cases} database={database} modeForDatabase={modeForDatabase} />
  }

  async function handleApprove(action) {
    const id = action.id
    setActionMsg(null)
    try {
      const dbParam = action?.database_name
        ? `?database=${encodeURIComponent(action.database_name)}` : ''
      const res = await fetch(
        `/api/v1/actions/${id}/approve${dbParam}`,
        {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
        },
      )
      const json = await res.json()
      const result = approvalOutcomeMessage(res.status, json)
      if (result.ok) toast.success(result.text)
      else toast.error(result.text)
      setActionMsg({ type: result.ok ? 'success' : 'error', text: result.text })
      refetch()
      refetchPendingCount()
    } catch (err) {
      toast.error(`Approve failed: ${err.message}`)
      setActionMsg({ type: 'error', text: err.message })
    }
  }

  async function handleReject(action) {
    const id = action.id
    setActionMsg(null)
    const reason = rejectReason.trim()
    if (!reason) {
      setActionMsg({ type: 'error',
        text: 'A rejection reason is required' })
      return
    }
    try {
      const dbParam = action?.database_name
        ? `?database=${encodeURIComponent(action.database_name)}` : ''
      const res = await fetch(
        `/api/v1/actions/${id}/reject${dbParam}`,
        {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ reason }),
        },
      )
      const json = await res.json()
      if (json.ok) {
        toast.success(`Action ${id} rejected`)
        setActionMsg({ type: 'success',
          text: `Action ${id} rejected` })
      } else {
        toast.error(json.error || 'Reject failed')
        setActionMsg({ type: 'error',
          text: json.error || 'Reject failed' })
      }
      setRejectId(null)
      setRejectReason('')
      refetch()
      refetchPendingCount()
    } catch (err) {
      toast.error(`Reject failed: ${err.message}`)
      setActionMsg({ type: 'error', text: err.message })
    }
  }

  const columns = [
    { key: 'action_risk', label: 'Risk',
      render: r => {
        const riskMap = {
          safe: { label: 'Low Risk', color: 'var(--green)' },
          moderate: {
            label: 'Moderate Risk', color: 'var(--yellow)',
          },
          high: { label: 'High Risk', color: 'var(--red)' },
          high_risk: { label: 'High Risk', color: 'var(--red)' },
        }
        const risk = actionRisk(r)
        const info = riskMap[risk] || {
          label: risk,
          color: 'var(--text-secondary)',
        }
        return (
          <span style={{ color: info.color }}>
            {info.label}
          </span>
        )
      },
    },
    { key: 'database_name', label: 'Database' },
    { key: 'finding_id', label: 'Finding' },
    ...(actions.some(r => r.recommendation_id)
      ? [{
        key: 'recommendation_id', label: 'Recommendation',
        render: r => revisionPin(r),
      }] : []),
    ...(actions.some(r => r.policy_decision)
      ? [{ key: 'policy_decision', label: 'Policy' }] : []),
    ...(actions.some(r => r.lifecycle_state || r.cooldown_until)
      ? [{
        key: 'lifecycle_state', label: 'Lifecycle',
        render: r => lifecycleStatus(r),
      }] : []),
    { key: 'availability', label: 'Availability', render: r => {
      const reason = modeForDatabase?.(rowDatabase(r, database)) === 'observation'
        ? 'Database changes are disabled in Observation mode.'
        : blockerText(r.defer_reason || r.blocked_reason)
      return <span style={{ color: reason ? 'var(--yellow)' : 'var(--text-secondary)' }}>
        {reason || (r.eligible === false ? 'Cannot run yet; eligibility reason unavailable.' : 'Awaiting your decision; safety checks run again before execution.')}
      </span>
    } },
    { key: 'proposed_sql', label: 'SQL Preview',
      render: r => {
        const sql = (r.proposed_sql || '').replace(/\s+/g, ' ').trim()
        return sql.length > 90 ? sql.slice(0, 90) + '...' : sql
      } },
    {
      key: 'proposed_at', label: 'Proposed',
      render: r => <LiveTimeAgo timestamp={r.proposed_at} />,
    },
    {
      key: 'actions', label: 'Decision',
      render: r => (
        <div className="space-y-2">
          <div className="flex flex-wrap gap-2">
            <button onClick={() => handleApprove(r)} data-testid="approve-button"
              disabled={r.eligible === false || modeForDatabase?.(rowDatabase(r, database)) === 'observation'}
              title={r.eligible === false ? (r.defer_reason || r.blocked_reason || 'Action is not eligible') : undefined}
              className="px-2 py-1 rounded text-xs" style={{ background: 'var(--green)', color: '#fff' }}>
              Approve and run
            </button>
            <button onClick={() => setRejectId(rejectId === actionRowKey(r) ? null : actionRowKey(r))}
              data-testid="reject-button" className="px-2 py-1 rounded text-xs"
              style={{ background: 'var(--red)', color: '#fff' }}>Reject</button>
          </div>
          {rejectId === actionRowKey(r) && <div className="flex flex-wrap gap-2">
            <input value={rejectReason} aria-label="Reason for rejection"
              onChange={e => setRejectReason(e.target.value)} placeholder="Reason for rejection"
              className="px-2 py-1 rounded text-sm" style={{ background: 'var(--bg-primary)',
                color: 'var(--text-primary)', border: '1px solid var(--border)' }} />
            <button onClick={() => handleReject(r)} disabled={!rejectReason.trim()}
              className="px-2 py-1 rounded text-xs" style={{ background: 'var(--red)', color: '#fff' }}>
              Confirm Reject</button>
          </div>}
        </div>
      ),
    },
  ]


  return (
    <div className="space-y-3">
      {hasCards && (
        <button data-testid="pending-view-cards" onClick={() => setView('cards')}
          className="px-2 py-1 rounded text-xs"
          style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
          Card view
        </button>
      )}
      <p data-testid="pending-help-text"
        className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        Review the evidence, exact SQL and recovery plan before deciding.
        Approve and run requests immediate execution after current safety checks; it does not establish that the change helped.
      </p>
      {actionMsg && (
        <div className="p-2 rounded text-sm"
          style={{
            background: actionMsg.type === 'success'
              ? 'var(--green)' : 'var(--red)',
            color: '#fff',
            opacity: 0.9,
          }}>
          {actionMsg.text}
        </div>
      )}
      <PendingErrors errors={data?.errors} />
      {actions.length === 0 ? (
        <EmptyState message="Nothing needs your approval in this view. Review Proposed changes for opportunities and blocked work." />
      ) : <DataTable data-testid="pending-actions-table"
        columns={columns} rows={actions} expandable rowKey={actionRowKey}
        renderExpanded={row => (
          <div className="space-y-3">
            <CaseRelation row={row} cases={cases} database={database} />
            <ApprovalCardDetail action={row} onDecided={refreshAll}
              mode={modeForDatabase?.(rowDatabase(row, database))} />
            <div>
              <div className="text-xs font-medium mb-1"
                style={{ color: 'var(--text-secondary)' }}>
                Proposed SQL
              </div>
              <SQLBlock sql={row.proposed_sql} />
            </div>
            {row.rollback_sql && (
              <div>
                <div className="text-xs font-medium mb-1"
                  style={{ color: 'var(--text-secondary)' }}>
                  Rollback SQL
                </div>
                <SQLBlock sql={row.rollback_sql} />
              </div>
            )}
            {row.script_output && (
              <div>
                <div className="text-xs font-medium mb-1"
                  style={{ color: 'var(--text-secondary)' }}>
                  Migration script
                </div>
                <div className="text-xs mb-2"
                  style={{ color: 'var(--text-secondary)' }}>
                  {row.script_output.filename}
                </div>
                <SQLBlock sql={row.script_output.migration_sql} />
                {row.script_output.rollback_sql && (
                  <div className="mt-2">
                    <div className="text-xs font-medium mb-1"
                      style={{ color: 'var(--text-secondary)' }}>
                      Rollback script
                    </div>
                    <SQLBlock sql={row.script_output.rollback_sql} />
                  </div>
                )}
                {(row.script_output.verification_sql || []).length > 0 && (
                  <div className="mt-2">
                    <div className="text-xs font-medium mb-1"
                      style={{ color: 'var(--text-secondary)' }}>
                      Verification SQL
                    </div>
                    {row.script_output.verification_sql.map(sql => (
                      <SQLBlock key={sql} sql={sql} />
                    ))}
                  </div>
                )}
                {(row.script_output.pr_title ||
                  row.script_output.pr_body) && (
                  <div className="mt-2 text-xs"
                    style={{ color: 'var(--text-secondary)' }}>
                    <div className="font-medium"
                      style={{ color: 'var(--text-primary)' }}>
                      PR / CI output
                    </div>
                    {row.script_output.pr_title && (
                      <div>{row.script_output.pr_title}</div>
                    )}
                    {row.script_output.pr_body && (
                      <div>{row.script_output.pr_body}</div>
                    )}
                  </div>
                )}
              </div>
            )}
            <LifecycleDetails row={row} />
          </div>
        )}
      />}
    </div>
  )
}

function LifecycleDetails({ row }) {
  const expiresAt = formatActionTime(row.expires_at)
  const cooldownUntil = formatActionTime(row.cooldown_until)
  const guardrails = row.guardrails || []
  const rollbackClass = rollbackClassLabel(row.rollback_class)
  if (!expiresAt && !cooldownUntil && guardrails.length === 0 &&
    !row.blocked_reason && !row.attempt_count && !rollbackClass) {
    return null
  }
  return (
    <div className="space-y-2 text-xs"
      style={{ color: 'var(--text-secondary)' }}>
      <div className="flex flex-wrap gap-2">
        {row.attempt_count > 0 && <span>Attempts: {row.attempt_count}</span>}
        {expiresAt && <span>Expires: {expiresAt}</span>}
        {cooldownUntil && <span>Cooldown until: {cooldownUntil}</span>}
        {rollbackClass && <span>{rollbackClass}</span>}
        {row.verification_status && (
          <span>Verification: {row.verification_status}</span>
        )}
      </div>
      {row.blocked_reason && (
        <div style={{ color: 'var(--text-primary)' }}>
          {row.blocked_reason}
        </div>
      )}
      {row.defer_reason && row.defer_reason !== row.blocked_reason && (
        <div style={{ color: 'var(--text-primary)' }}>
          {row.defer_reason}
        </div>
      )}
      {guardrails.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {guardrails.map(g => (
            <span key={g} className="rounded px-1.5 py-0.5"
              style={{ border: '1px solid var(--border)' }}>
              {g}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

function CaseRelation({ row, cases, database }) {
  const linked = relatedCase(row, cases, database)
  return <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
    {linked ? <a href={caseHref(linked, database)} className="underline"
      style={{ color: 'var(--accent)' }}>Review case: {linked.title}</a>
      : <>Case relation unavailable. <a href={caseHref(null, rowDatabase(row, database))}
        className="underline" style={{ color: 'var(--accent)' }}>View Cases for this database</a></>}
  </p>
}
