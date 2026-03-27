import { useAPI } from '../hooks/useAPI'
import { StatusDot } from '../components/StatusDot'
import { SeverityBadge } from '../components/SeverityBadge'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'

function StatCard({ label, value, color }) {
  return (
    <div className="rounded p-4"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div className="text-xs mb-1"
        style={{ color: 'var(--text-secondary)' }}>{label}</div>
      <div className="text-2xl font-bold"
        style={{ color: color || 'var(--text-primary)' }}>{value}</div>
    </div>
  )
}

export function Dashboard({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const { data, loading, error, refetch } = useAPI('/api/v1/databases')
  const findings = useAPI(`/api/v1/findings${dbParam}&limit=5`)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />
  if (!data) return null

  const { summary, databases } = data

  return (
    <div className="space-y-6">
      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        <StatCard label="Databases"
          value={summary.total_databases} />
        <StatCard label="Healthy"
          value={summary.healthy} color="var(--green)" />
        <StatCard label="Degraded" value={summary.degraded}
          color={summary.degraded > 0
            ? 'var(--red)' : 'var(--green)'} />
        <StatCard label="Critical Findings"
          value={summary.total_critical}
          color={summary.total_critical > 0
            ? 'var(--red)' : 'var(--text-primary)'} />
      </div>

      <div className="rounded p-4"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h2 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Databases
        </h2>
        <div className="space-y-2">
          {databases.map(db => (
            <div key={db.name}
              className="flex items-center gap-3 p-2 rounded"
              style={{ background: 'var(--bg-primary)' }}>
              <StatusDot connected={db.status.connected}
                error={db.status.error} />
              <span className="font-medium flex-1">{db.name}</span>
              <span className="text-xs px-2 py-0.5 rounded"
                style={{
                  background: 'var(--bg-hover)',
                  color: 'var(--text-secondary)',
                }}>
                Score: {db.status.health_score}
              </span>
              {db.status.findings_critical > 0 && (
                <SeverityBadge severity="critical" />
              )}
              {db.status.findings_warning > 0 && (
                <SeverityBadge severity="warning" />
              )}
            </div>
          ))}
        </div>
      </div>

      {findings.data?.findings?.length > 0 && (
        <div className="rounded p-4"
          style={{
            background: 'var(--bg-card)',
            border: '1px solid var(--border)',
          }}>
          <h2 className="text-sm font-medium mb-3"
            style={{ color: 'var(--text-secondary)' }}>
            Recent Findings
          </h2>
          <div className="space-y-2">
            {findings.data.findings.map((f, i) => (
              <div key={i}
                className="flex items-center gap-3 p-2 rounded"
                style={{ background: 'var(--bg-primary)' }}>
                <SeverityBadge severity={f.severity} />
                <span className="flex-1 text-sm">{f.title}</span>
                <span className="text-xs"
                  style={{ color: 'var(--text-secondary)' }}>
                  {f.database_name}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
