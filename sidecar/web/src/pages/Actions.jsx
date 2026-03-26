import { useAPI } from '../hooks/useAPI'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { TimeAgo } from '../components/TimeAgo'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

export function Actions({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const { data, loading, error, refetch } =
    useAPI(`/api/v1/actions${dbParam}`)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const actions = data?.actions || []

  const columns = [
    { key: 'action_type', label: 'Type' },
    {
      key: 'outcome', label: 'Outcome',
      render: r => (
        <span style={{
          color: r.outcome === 'success'
            ? 'var(--green)' : 'var(--red)',
        }}>
          {r.outcome}
        </span>
      ),
    },
    { key: 'database_name', label: 'Database' },
    {
      key: 'executed_at', label: 'When',
      render: r => <TimeAgo timestamp={r.executed_at} />,
    },
  ]

  if (actions.length === 0) {
    return <EmptyState message="No actions recorded" />
  }

  return (
    <DataTable columns={columns} rows={actions} expandable
      renderExpanded={row => (
        <div className="space-y-3">
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
        </div>
      )}
    />
  )
}
