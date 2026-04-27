import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { SeverityBadge } from '../components/SeverityBadge'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

const emptyMessages = {
  active: 'No active incidents. All systems healthy.',
  resolved: 'No resolved incidents yet.',
}

const sourceLabels = {
  deterministic: 'Metric',
  log_deterministic: 'Log',
  self_action: 'Self-Action',
  manual_review_required: 'Manual Review',
  tier2_llm: 'AI Correlation',
  llm: 'AI Correlation',
  schema_advisor: 'Schema Advisor',
  schema_lint: 'Schema Lint',
  n_plus_one: 'N+1 Query',
}

const riskStyles = {
  safe: { background: 'var(--green)', label: 'Safe' },
  low: { background: 'var(--green)', label: 'Low' },
  moderate: {
    background: 'var(--yellow, #eab308)', label: 'Moderate',
  },
  medium: {
    background: 'var(--yellow, #eab308)', label: 'Medium',
  },
  high_risk: { background: 'var(--red)', label: 'High Risk' },
  high: { background: 'var(--red)', label: 'High' },
}

function timeAgo(dateStr) {
  if (!dateStr) return '-'
  const seconds = Math.floor(
    (Date.now() - new Date(dateStr).getTime()) / 1000,
  )
  if (seconds < 0) return 'just now'
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

function truncate(str, max) {
  if (!str) return '-'
  return str.length > max ? str.slice(0, max) + '\u2026' : str
}

export function IncidentsPage({ database, user }) {
  const [status, setStatus] = useState('active')
  const [severity, setSeverity] = useState('')
  const dbParam = database && database !== 'all'
    ? `&database=${database}` : ''
  const sevParam = severity ? `&severity=${severity}` : ''
  const url =
    `/api/v1/incidents?status=${status}${dbParam}${sevParam}`
  const { data, loading, error, refetch } = useAPI(url)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const incidents = data?.incidents || []

  const columns = [
    {
      key: 'severity', label: 'Severity',
      render: r => <SeverityBadge severity={r.severity} />,
    },
    {
      key: 'root_cause', label: 'Root Cause',
      render: r => truncate(r.root_cause, 80),
    },
    {
      key: 'source', label: 'Source',
      render: r => sourceLabels[r.source] || r.source,
    },
    {
      key: 'database_name', label: 'Database',
      render: r => r.fleet_database_name || r.database_name || '-',
    },
    { key: 'occurrence_count', label: 'Count' },
    {
      key: 'detected_at', label: 'Detected',
      render: r => timeAgo(r.detected_at),
    },
  ]

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        {['active', 'resolved'].map(s => (
          <button key={s} onClick={() => setStatus(s)}
            className="px-3 py-1.5 rounded text-sm"
            style={{
              background: status === s
                ? 'var(--accent)' : 'var(--bg-card)',
              color: status === s ? '#fff' : 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            {s.charAt(0).toUpperCase() + s.slice(1)}
          </button>
        ))}
        <select value={severity}
          onChange={e => setSeverity(e.target.value)}
          data-testid="severity-filter"
          className="px-3 py-1.5 rounded text-sm ml-auto"
          style={{
            background: 'var(--bg-card)',
            color: 'var(--text-primary)',
            border: '1px solid var(--border)',
          }}>
          <option value="">All severities</option>
          <option value="critical">Critical</option>
          <option value="warning">Warning</option>
          <option value="info">Info</option>
        </select>
      </div>

      {incidents.length === 0 ? (
        <EmptyState message={emptyMessages[status]} />
      ) : (
        <DataTable data-testid="incidents-table"
          columns={columns} rows={incidents} expandable
          renderExpanded={row => (
            <IncidentDetail row={row}
              database={
                row.fleet_database_name || row.database_name || database
              }
              canResolve={
                status === 'active'
                && (user?.role === 'admin'
                  || user?.role === 'operator')
              }
              onResolved={refetch} />
          )}
        />
      )}

      <div className="text-xs"
        data-testid="incidents-count"
        style={{ color: 'var(--text-secondary)' }}>
        {data?.total || 0} total incidents
      </div>
    </div>
  )
}

function IncidentDetail({ row, database, canResolve, onResolved }) {
  const [resolving, setResolving] = useState(false)
  const [result, setResult] = useState(null)

  async function handleResolve() {
    setResolving(true)
    setResult(null)
    try {
      const dbParam = database && database !== 'all'
        ? `?database=${encodeURIComponent(database)}` : ''
      const res = await fetch(
        `/api/v1/incidents/${row.id}/resolve${dbParam}`,
        {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ reason: '' }),
        },
      )
      if (!res.ok) throw new Error('Failed to resolve')
      setResult({ type: 'success', text: 'Incident resolved' })
      if (onResolved) onResolved()
    } catch (err) {
      setResult({ type: 'error', text: err.message })
    } finally {
      setResolving(false)
    }
  }

  const chain = row.causal_chain || row.chain || []
  const risk = row.action_risk && riskStyles[row.action_risk]
  const signalIds = row.signal_ids || []
  const affectedObjects = row.affected_objects || []

  return (
    <div className="space-y-3">
      {/* Full root cause */}
      <p className="text-sm"
        style={{ color: 'var(--text-primary)' }}>
        {row.root_cause}
      </p>

      {/* Causal chain */}
      {chain.length > 0 && (
        <div>
          <div className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Causal Chain
          </div>
          <ol className="space-y-1 pl-4 text-xs list-decimal"
            style={{ color: 'var(--text-primary)' }}>
            {chain
              .sort((a, b) => a.order - b.order)
              .map((step, i) => (
                <li key={i} className="pl-1">
                  <span className="font-medium">{step.signal}</span>
                  {' \u2014 '}{step.description}
                  {step.evidence && (
                    <span className="ml-1"
                      style={{ color: 'var(--text-secondary)' }}>
                      ({step.evidence})
                    </span>
                  )}
                </li>
              ))}
          </ol>
        </div>
      )}

      {/* Recommended SQL */}
      {row.recommended_sql && (
        <div>
          <div className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Recommended SQL
          </div>
          <SQLBlock sql={row.recommended_sql} />
        </div>
      )}

      {/* Rollback SQL */}
      {row.rollback_sql && (
        <div>
          <div className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Rollback SQL
          </div>
          <SQLBlock sql={row.rollback_sql} />
        </div>
      )}

      {/* Metadata row */}
      <div className="flex gap-2 flex-wrap items-center">
        {risk && (
          <span className="text-xs font-medium px-2 py-0.5 rounded"
            style={{ background: risk.background, color: '#fff' }}>
            {risk.label}
          </span>
        )}
        {row.confidence != null && (
          <span className="text-xs px-2 py-0.5 rounded"
            style={{
              background: 'var(--bg-primary)',
              color: 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            Confidence: {Math.round(row.confidence * 100)}%
          </span>
        )}
        {row.escalated_at && (
          <span className="text-xs font-medium px-2 py-0.5 rounded"
            style={{
              background: 'var(--red)',
              color: '#fff',
            }}>
            Escalated
          </span>
        )}
      </div>

      {/* Signal IDs */}
      {signalIds.length > 0 && (
        <div>
          <div className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Signal IDs
          </div>
          <div className="flex gap-1 flex-wrap">
            {signalIds.map((sid, i) => (
              <span key={i}
                className="text-xs px-1.5 py-0.5 rounded"
                style={{
                  background: 'var(--bg-primary)',
                  color: 'var(--text-secondary)',
                  border: '1px solid var(--border)',
                }}>
                {sid}
              </span>
            ))}
          </div>
        </div>
      )}

      {/* Affected objects */}
      {affectedObjects.length > 0 && (
        <div>
          <div className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Affected Objects
          </div>
          <ul className="text-xs pl-4 list-disc"
            style={{ color: 'var(--text-primary)' }}>
            {affectedObjects.map((obj, i) => (
              <li key={i}>{obj}</li>
            ))}
          </ul>
        </div>
      )}

      {/* Result feedback */}
      {result && (
        <div className="p-2 rounded text-sm"
          style={{
            background: result.type === 'success'
              ? 'var(--green)' : 'var(--red)',
            color: '#fff',
            opacity: 0.9,
          }}>
          {result.text}
        </div>
      )}

      {/* Resolve button */}
      {canResolve && (
        <button onClick={handleResolve} disabled={resolving}
          className="px-3 py-1.5 rounded text-sm"
          style={{
            background: 'var(--accent)',
            color: '#fff',
            opacity: resolving ? 0.5 : 1,
          }}>
          {resolving ? 'Resolving...' : 'Resolve Incident'}
        </button>
      )}
    </div>
  )
}
