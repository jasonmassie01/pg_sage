import { useState } from 'react'
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

function actionStatus(row) {
  return row.status || row.action_status || row.outcome || 'unknown'
}

function actionRisk(row) {
  return row.risk_tier || row.action_risk || row.risk || 'unknown'
}

function verificationStatus(row) {
  return row.verification_status || row.status || 'not_started'
}

function lifecycleStatus(row) {
  return row.lifecycle_state || row.status || 'ready'
}

function formatActionTime(value) {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return null
  return date.toLocaleString()
}

export function Actions({ database, user }) {
  const [tab, setTab] = useState('executed')
  const range = useTimeRange()
  const canReview = user?.role === 'admin' || user?.role === 'operator'
  const activeTab = canReview ? tab : 'executed'
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''

  const { data, loading, error, refetch } =
    useAPI(withTimeRange(`/api/v1/actions${dbParam}`, range))
  const {
    data: pendingData,
    loading: pendingLoading,
    error: pendingError,
    refetch: pendingRefetch,
  } = useAPI(canReview ? `/api/v1/actions/pending${dbParam}` : null)
  useLiveRefetch(['actions'], refetch)
  useLiveRefetch(['actions'], canReview ? pendingRefetch : null)

  if (activeTab === 'executed') {
    return (
      <div className="space-y-4">
        <TabBar tab={activeTab} setTab={setTab}
          pendingCount={pendingData?.total || 0}
          canReview={canReview} />
        <ExecutedTab data={data} loading={loading}
          error={error} refetch={refetch} user={user} />
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <TabBar tab={activeTab} setTab={setTab}
        pendingCount={pendingData?.total || 0}
        canReview={canReview} />
      <PendingTab data={pendingData}
        loading={pendingLoading}
        error={pendingError}
        refetch={pendingRefetch} />
    </div>
  )
}

function TabBar({ tab, setTab, pendingCount, canReview }) {
  const tabs = [
    { key: 'executed', label: 'Executed' },
  ]
  if (canReview) {
    tabs.push({ key: 'pending', label: 'Pending Approval' })
  }

  return (
    <div className="flex gap-2">
      {tabs.map(t => (
        <button key={t.key} onClick={() => setTab(t.key)}
          data-testid={`actions-tab-${t.key}`}
          className="px-3 py-1.5 rounded text-sm"
          style={{
            background: tab === t.key
              ? 'var(--accent)' : 'var(--bg-card)',
            color: tab === t.key
              ? '#fff' : 'var(--text-secondary)',
            border: '1px solid var(--border)',
          }}>
          {t.label}
          {t.key === 'pending' && pendingCount > 0 && (
            <span className="ml-1.5 px-1.5 py-0.5 rounded-full text-xs"
              style={{
                background: 'var(--red)',
                color: '#fff',
              }}>
              {pendingCount}
            </span>
          )}
        </button>
      ))}
    </div>
  )
}

function ExecutedTab({ data, loading, error, refetch, user }) {
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

  const actions = data?.actions || []

  const outcomeStyle = outcome => {
    switch (outcome) {
    case 'success':
      return { bg: 'rgba(34,197,94,0.15)', color: 'var(--green)',
        label: 'Success' }
    case 'failed':
      return { bg: 'rgba(239,68,68,0.15)', color: 'var(--red)',
        label: 'Failed' }
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
        return (
          <span className="px-2 py-0.5 rounded-full text-xs
            font-medium inline-block"
            style={{ background: s.bg, color: s.color }}>
            {s.label}
          </span>
        )
      },
    },
    ...(actions.some(r => r.verification_status)
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
    const res = await fetch(`/api/v1/actions/${row.id}/rollback${dbParam}`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason: 'manual rollback from actions UI' }),
    })
    const json = await res.json()
    if (!res.ok || !json.ok) {
      toast.error(json.error || 'Rollback failed')
      return
    }
    toast.success(`Action ${row.id} rolled back`)
    refetch()
  }

  if (actions.length === 0) {
    return <EmptyState message="No actions have been executed yet. Actions will appear here as pg_sage works on your databases." />
  }

  return (
    <DataTable data-testid="executed-actions-table"
      columns={columns} rows={actions} expandable
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
              SQL Executed
            </div>
            <SQLBlock sql={row.sql_executed} />
          </div>
          {row.rollback_sql && (
            <div>
              <div className="text-xs font-medium mb-1"
                style={{ color: 'var(--text-secondary)' }}>
                Rollback SQL
              </div>
              <SQLBlock sql={row.rollback_sql} />
              {canRollback
                && (row.outcome === 'success'
                  || row.outcome === 'monitoring'
                  || row.outcome === 'pending') && (
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

function PendingTab({
  data, loading, error, refetch,
}) {
  const [rejectId, setRejectId] = useState(null)
  const [rejectReason, setRejectReason] = useState('')
  const [actionMsg, setActionMsg] = useState(null)
  const refetchPendingCount = usePendingActionsRefetch()
  const toast = useToast()

  if (loading) return <PendingSkeleton />
  if (error) return <ErrorBanner message={error}
    onRetry={refetch} />

  const actions = data?.pending || []

  async function handleApprove(id) {
    setActionMsg(null)
    try {
      const action = actions.find(a => a.id === id)
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
      if (json.ok) {
        toast.success(`Action ${id} approved and executed`)
        setActionMsg({ type: 'success',
          text: `Action ${id} approved and executed` })
      } else {
        toast.error(json.error || 'Approve failed')
        setActionMsg({ type: 'error',
          text: json.error || 'Approve failed' })
      }
      refetch()
      refetchPendingCount()
    } catch (err) {
      toast.error(`Approve failed: ${err.message}`)
      setActionMsg({ type: 'error', text: err.message })
    }
  }

  async function handleReject(id) {
    setActionMsg(null)
    const reason = rejectReason.trim()
    if (!reason) {
      setActionMsg({ type: 'error',
        text: 'A rejection reason is required' })
      return
    }
    try {
      const action = actions.find(a => a.id === id)
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
    ...(actions.some(r => r.policy_decision)
      ? [{ key: 'policy_decision', label: 'Policy' }] : []),
    ...(actions.some(r => r.lifecycle_state || r.cooldown_until)
      ? [{
        key: 'lifecycle_state', label: 'Lifecycle',
        render: r => lifecycleStatus(r),
      }] : []),
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
      key: 'actions', label: '',
      render: r => (
        <div className="flex gap-2">
          <button onClick={() => handleApprove(r.id)}
            data-testid="approve-button"
            className="px-2 py-1 rounded text-xs"
            style={{
              background: 'var(--green)',
              color: '#fff',
            }}>
            Approve
          </button>
          <button
            onClick={() => setRejectId(
              rejectId === r.id ? null : r.id)}
            data-testid="reject-button"
            className="px-2 py-1 rounded text-xs"
            style={{
              background: 'var(--red)',
              color: '#fff',
            }}>
            Reject
          </button>
        </div>
      ),
    },
  ]

  if (actions.length === 0) {
    return <EmptyState message="No actions waiting for approval. When pg_sage identifies improvements that need your OK, they'll appear here." />
  }

  return (
    <div className="space-y-3">
      <p data-testid="pending-help-text"
        className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        These actions are waiting for your review. Approve to
        execute, or reject with a reason.
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
      <DataTable data-testid="pending-actions-table"
        columns={columns} rows={actions} expandable
        renderExpanded={row => (
          <div className="space-y-3">
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
            <LifecycleDetails row={row} />
            {rejectId === row.id && (
              <div className="flex gap-2 items-center">
                <input
                  value={rejectReason}
                  onChange={e =>
                    setRejectReason(e.target.value)}
                  placeholder="Reason for rejection..."
                  className="px-2 py-1 rounded text-sm flex-1"
                  style={{
                    background: 'var(--bg-primary)',
                    color: 'var(--text-primary)',
                    border: '1px solid var(--border)',
                  }}
                />
                <button
                  onClick={() => handleReject(row.id)}
                  disabled={!rejectReason.trim()}
                  className="px-2 py-1 rounded text-xs"
                  style={{
                    background: 'var(--red)',
                    color: '#fff',
                    opacity: rejectReason.trim() ? 1 : 0.45,
                    cursor: rejectReason.trim() ? 'pointer' : 'not-allowed',
                  }}>
                  Confirm Reject
                </button>
              </div>
            )}
          </div>
        )}
      />
    </div>
  )
}

function LifecycleDetails({ row }) {
  const expiresAt = formatActionTime(row.expires_at)
  const cooldownUntil = formatActionTime(row.cooldown_until)
  const guardrails = row.guardrails || []
  if (!expiresAt && !cooldownUntil && guardrails.length === 0 &&
    !row.blocked_reason && !row.attempt_count) {
    return null
  }
  return (
    <div className="space-y-2 text-xs"
      style={{ color: 'var(--text-secondary)' }}>
      <div className="flex flex-wrap gap-2">
        {row.attempt_count > 0 && <span>Attempts: {row.attempt_count}</span>}
        {expiresAt && <span>Expires: {expiresAt}</span>}
        {cooldownUntil && <span>Cooldown until: {cooldownUntil}</span>}
        {row.verification_status && (
          <span>Verification: {row.verification_status}</span>
        )}
      </div>
      {row.blocked_reason && (
        <div style={{ color: 'var(--text-primary)' }}>
          {row.blocked_reason}
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
