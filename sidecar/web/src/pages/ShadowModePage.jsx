import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'

function dbParam(database) {
  return database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}`
    : ''
}

function Stat({ label, value }) {
  return (
    <div className="rounded border p-3"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="text-2xl font-semibold"
        style={{ color: 'var(--text-primary)' }}>
        {value}
      </div>
      <div className="text-xs mt-1"
        style={{ color: 'var(--text-secondary)' }}>
        {label}
      </div>
    </div>
  )
}

export function ShadowModePage({ database }) {
  const { data, loading, error, refetch } = useAPI(
    `/api/v1/shadow-report${dbParam(database)}`,
    30000,
  )

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  return (
    <section className="space-y-4" data-testid="shadow-mode-report">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Shadow Mode
        </h2>
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          What pg_sage would have handled under auto-safe policy.
        </p>
      </div>
      <div className="grid gap-3 md:grid-cols-4">
        <Stat label="Cases detected" value={data?.total_cases ?? 0} />
        <Stat label="Would auto-resolve"
          value={data?.would_auto_resolve ?? 0} />
        <Stat label="Needs approval" value={data?.requires_approval ?? 0} />
        <Stat label="Avoided toil"
          value={`${data?.estimated_toil_minutes ?? 0} min`} />
      </div>
      {(data?.blocked_reasons || []).length > 0 && (
        <div className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          Blocked: {data.blocked_reasons.join(', ')}
        </div>
      )}
    </section>
  )
}
