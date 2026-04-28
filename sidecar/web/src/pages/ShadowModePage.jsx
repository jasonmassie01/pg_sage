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
      {(data?.proof || []).length > 0 && (
        <div className="space-y-2">
          {data.proof.map(item => (
            <ShadowProofRow key={`${item.case_id}-${item.action_type}`}
              item={item} />
          ))}
        </div>
      )}
    </section>
  )
}

function ShadowProofRow({ item }) {
  return (
    <div className="rounded border p-3 text-sm"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="font-medium" style={{ color: 'var(--text-primary)' }}>
        {item.title}
      </div>
      <div className="mt-1 flex flex-wrap gap-2 text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        <span>{item.action_type}</span>
        {item.action_id && <span>Action: {item.action_id}</span>}
        <span>Policy: {item.policy_decision}</span>
        {item.lifecycle_state && (
          <span>Lifecycle: {item.lifecycle_state}</span>
        )}
        {item.status && <span>Status: {item.status}</span>}
        {item.verification_status && (
          <span>Verification: {item.verification_status}</span>
        )}
        {item.proposed_at && <span>Proposed: {item.proposed_at}</span>}
        <span>Toil: {item.estimated_toil_minutes} min</span>
        {item.blocked_reason && (
          <span>Blocked: {item.blocked_reason}</span>
        )}
      </div>
    </div>
  )
}
