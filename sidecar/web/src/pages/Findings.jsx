import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { SeverityBadge } from '../components/SeverityBadge'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

export function Findings({ database }) {
  const [status, setStatus] = useState('open')
  const [severity, setSeverity] = useState('')
  const dbParam = database && database !== 'all'
    ? `&database=${database}` : ''
  const sevParam = severity ? `&severity=${severity}` : ''
  const url =
    `/api/v1/findings?status=${status}${dbParam}${sevParam}&limit=50`
  const { data, loading, error, refetch } = useAPI(url)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const findings = data?.findings || []

  const columns = [
    {
      key: 'severity', label: 'Severity',
      render: r => <SeverityBadge severity={r.severity} />,
    },
    { key: 'category', label: 'Category' },
    { key: 'title', label: 'Title' },
    { key: 'database_name', label: 'Database' },
    { key: 'occurrence_count', label: 'Count' },
  ]

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        {['open', 'suppressed', 'resolved'].map(s => (
          <button key={s} onClick={() => setStatus(s)}
            className="px-3 py-1.5 rounded text-sm"
            style={{
              background: status === s
                ? 'var(--accent)' : 'var(--bg-card)',
              color: status === s ? '#fff' : 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            {s}
          </button>
        ))}
        <select value={severity}
          onChange={e => setSeverity(e.target.value)}
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

      {findings.length === 0 ? (
        <EmptyState message={`No ${status} findings`} />
      ) : (
        <DataTable columns={columns} rows={findings} expandable
          renderExpanded={row => (
            <div className="space-y-3">
              <p className="text-sm"
                style={{ color: 'var(--text-secondary)' }}>
                {row.recommendation}
              </p>
              {row.recommended_sql && (
                <div>
                  <div className="text-xs font-medium mb-1"
                    style={{ color: 'var(--text-secondary)' }}>
                    Recommended SQL
                  </div>
                  <SQLBlock sql={row.recommended_sql} />
                </div>
              )}
              {row.detail && (
                <div>
                  <div className="text-xs font-medium mb-1"
                    style={{ color: 'var(--text-secondary)' }}>
                    Detail
                  </div>
                  <pre className="text-xs p-2 rounded overflow-auto"
                    style={{
                      background: 'var(--bg-primary)',
                      color: 'var(--text-secondary)',
                    }}>
                    {JSON.stringify(row.detail, null, 2)}
                  </pre>
                </div>
              )}
            </div>
          )}
        />
      )}

      <div className="text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        {data?.total || 0} total findings
      </div>
    </div>
  )
}
