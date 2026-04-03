import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { TimeAgo } from '../components/TimeAgo'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

export function Actions({ database, user }) {
  const [tab, setTab] = useState('executed')
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''

  const { data, loading, error, refetch } =
    useAPI(`/api/v1/actions${dbParam}`)
  const {
    data: pendingData,
    loading: pendingLoading,
    error: pendingError,
    refetch: pendingRefetch,
  } = useAPI('/api/v1/actions/pending')

  if (tab === 'executed') {
    return (
      <div className="space-y-4">
        <TabBar tab={tab} setTab={setTab}
          pendingCount={pendingData?.total || 0} />
        <ExecutedTab data={data} loading={loading}
          error={error} refetch={refetch} />
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <TabBar tab={tab} setTab={setTab}
        pendingCount={pendingData?.total || 0} />
      <PendingTab data={pendingData}
        loading={pendingLoading}
        error={pendingError}
        refetch={pendingRefetch} user={user} />
    </div>
  )
}

function TabBar({ tab, setTab, pendingCount }) {
  const tabs = [
    { key: 'executed', label: 'Executed' },
    { key: 'pending', label: 'Pending Approval' },
  ]

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

function ExecutedTab({ data, loading, error, refetch }) {
  if (loading) return <LoadingSpinner />
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
        const s = outcomeStyle(r.outcome)
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
      key: 'executed_at', label: 'When',
      render: r => <TimeAgo timestamp={r.executed_at} />,
    },
  ]

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
            </div>
          )}
          <div className="flex gap-4 text-xs" style={{
            color: 'var(--text-secondary)',
          }}>
            {row.finding_id && (
              <span>Finding #{row.finding_id}</span>
            )}
            {row.measured_at && (
              <span>Verified: <TimeAgo
                timestamp={row.measured_at} /></span>
            )}
          </div>
        </div>
      )}
    />
  )
}

function PendingTab({
  data, loading, error, refetch, user,
}) {
  const [rejectId, setRejectId] = useState(null)
  const [rejectReason, setRejectReason] = useState('')
  const [actionMsg, setActionMsg] = useState(null)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error}
    onRetry={refetch} />

  const actions = data?.pending || []

  async function handleApprove(id) {
    setActionMsg(null)
    try {
      const res = await fetch(
        `/api/v1/actions/${id}/approve`,
        { method: 'POST', credentials: 'include' },
      )
      const json = await res.json()
      if (json.ok) {
        setActionMsg({ type: 'success',
          text: `Action ${id} approved and executed` })
      } else {
        setActionMsg({ type: 'error',
          text: json.error || 'Approve failed' })
      }
      refetch()
    } catch (err) {
      setActionMsg({ type: 'error', text: err.message })
    }
  }

  async function handleReject(id) {
    setActionMsg(null)
    try {
      const res = await fetch(
        `/api/v1/actions/${id}/reject`,
        {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ reason: rejectReason }),
        },
      )
      const json = await res.json()
      if (json.ok) {
        setActionMsg({ type: 'success',
          text: `Action ${id} rejected` })
      } else {
        setActionMsg({ type: 'error',
          text: json.error || 'Reject failed' })
      }
      setRejectId(null)
      setRejectReason('')
      refetch()
    } catch (err) {
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
        const info = riskMap[r.action_risk] || {
          label: r.action_risk,
          color: 'var(--text-secondary)',
        }
        return (
          <span style={{ color: info.color }}>
            {info.label}
          </span>
        )
      },
    },
    { key: 'finding_id', label: 'Finding' },
    {
      key: 'proposed_at', label: 'Proposed',
      render: r => <TimeAgo timestamp={r.proposed_at} />,
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
                  className="px-2 py-1 rounded text-xs"
                  style={{
                    background: 'var(--red)',
                    color: '#fff',
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
