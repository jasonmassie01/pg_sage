import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

export function DatabasePage({ database }) {
  const db = database && database !== 'all' ? database : null
  const { data, loading, error, refetch } = useAPI(
    db ? `/api/v1/snapshots/latest?database=${db}` : null, 0
  )

  if (!db) {
    return <EmptyState message="Select a database from the picker above" />
  }
  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  return (
    <div className="space-y-6">
      <div className="rounded p-4"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h2 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Database: {db}
        </h2>
        {data?.snapshot ? (
          <pre className="text-xs overflow-auto"
            style={{ color: 'var(--text-secondary)' }}>
            {JSON.stringify(data.snapshot, null, 2)}
          </pre>
        ) : (
          <EmptyState message="No snapshot data available yet" />
        )}
      </div>
    </div>
  )
}
