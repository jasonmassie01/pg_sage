import { useAPI } from '../hooks/useAPI'
import { StatusDot } from '../components/StatusDot'
import { SeverityBadge } from '../components/SeverityBadge'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import {
  CheckCircle, Clock, ListChecks, Server,
} from 'lucide-react'

function StatCard({ label, value, color, 'data-testid': testId }) {
  return (
    <div className="rounded p-4"
      data-testid={testId}
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

function HealthHero({ summary }) {
  const healthy = summary.degraded === 0
    && summary.total_critical === 0

  if (healthy) {
    return (
      <div data-testid="health-hero"
        className="rounded p-4 flex items-center gap-3"
        style={{
          background: 'rgba(34, 197, 94, 0.08)',
          border: '1px solid rgba(34, 197, 94, 0.3)',
        }}>
        <CheckCircle size={28} style={{ color: 'var(--green)' }} />
        <div>
          <div className="font-semibold"
            style={{ color: 'var(--green)' }}>
            All Systems Healthy
          </div>
          <div className="text-sm"
            style={{ color: 'var(--text-secondary)' }}>
            pg_sage is monitoring {summary.total_databases}{' '}
            database(s). No issues detected.
          </div>
        </div>
      </div>
    )
  }

  const parts = []
  if (summary.total_critical > 0) {
    parts.push(
      `${summary.total_critical} critical finding`
      + (summary.total_critical > 1 ? 's' : ''),
    )
  }
  if (summary.degraded > 0) {
    parts.push(
      `${summary.degraded} degraded database`
      + (summary.degraded > 1 ? 's' : ''),
    )
  }
  const issueCount = summary.total_critical + summary.degraded
  const breakdown = parts.length > 0
    ? parts.join(' across ')
    : 'Check findings for details.'

  return (
    <div data-testid="health-hero"
      className="rounded p-4 flex items-center gap-3"
      style={{
        background: summary.total_critical > 0
          ? 'rgba(239, 68, 68, 0.08)'
          : 'rgba(245, 158, 11, 0.08)',
        border: summary.total_critical > 0
          ? '1px solid rgba(239, 68, 68, 0.3)'
          : '1px solid rgba(245, 158, 11, 0.3)',
      }}>
      <Server size={28}
        style={{
          color: summary.total_critical > 0
            ? 'var(--red)' : 'var(--yellow)',
        }} />
      <div>
        <div className="font-semibold"
          style={{
            color: summary.total_critical > 0
              ? 'var(--red)' : 'var(--yellow)',
          }}>
          {issueCount} Issue{issueCount !== 1 ? 's' : ''}{' '}
          Need Attention
        </div>
        <div className="text-sm"
          style={{ color: 'var(--text-secondary)' }}>
          {breakdown}
        </div>
      </div>
    </div>
  )
}

function OnboardingWelcome() {
  return (
    <div data-testid="onboarding-welcome"
      className="rounded p-8 text-center"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <h1 className="text-2xl font-bold mb-2"
        style={{ color: 'var(--text-primary)' }}>
        Welcome to pg_sage
      </h1>
      <p className="mb-6"
        style={{ color: 'var(--text-secondary)' }}>
        Your agentic Postgres DBA.
        {' '}Let&apos;s get started.
      </p>
      <div className="space-y-4 text-left max-w-md mx-auto mb-8">
        <div className="flex items-start gap-3">
          <span className="flex-shrink-0 w-7 h-7 rounded-full
            flex items-center justify-center text-sm font-bold"
            style={{
              background: 'var(--bg-hover)',
              color: 'var(--text-primary)',
            }}>1</span>
          <div className="pt-0.5">
            <a href="#/manage-databases"
              className="font-medium underline"
              style={{ color: 'var(--blue, #3b82f6)' }}>
              Add your first database
            </a>
          </div>
        </div>
        <div className="flex items-start gap-3">
          <span className="flex-shrink-0 w-7 h-7 rounded-full
            flex items-center justify-center text-sm font-bold"
            style={{
              background: 'var(--bg-hover)',
              color: 'var(--text-primary)',
            }}>2</span>
          <div className="flex items-center gap-2 pt-0.5">
            <Clock size={16}
              style={{ color: 'var(--text-secondary)' }} />
            <span style={{ color: 'var(--text-secondary)' }}>
              pg_sage will automatically start monitoring
            </span>
          </div>
        </div>
        <div className="flex items-start gap-3">
          <span className="flex-shrink-0 w-7 h-7 rounded-full
            flex items-center justify-center text-sm font-bold"
            style={{
              background: 'var(--bg-hover)',
              color: 'var(--text-primary)',
            }}>3</span>
          <div className="flex items-center gap-2 pt-0.5">
            <ListChecks size={16}
              style={{ color: 'var(--text-secondary)' }} />
            <span style={{ color: 'var(--text-secondary)' }}>
              Review recommendations as they come in
            </span>
          </div>
        </div>
      </div>
      <a href="#/manage-databases"
        className="inline-block px-6 py-2 rounded font-medium"
        style={{
          background: 'var(--blue, #3b82f6)',
          color: '#fff',
        }}>
        Add Database
      </a>
    </div>
  )
}

function StatusLabel({ connected, error }) {
  if (!connected) {
    return (
      <span className="text-xs" style={{ color: 'var(--red)' }}>
        Disconnected
      </span>
    )
  }
  if (error) {
    return (
      <span className="text-xs"
        style={{ color: 'var(--yellow)' }}>
        Warning
      </span>
    )
  }
  return (
    <span className="text-xs" style={{ color: 'var(--green)' }}>
      Connected
    </span>
  )
}

export function Dashboard({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const { data, loading, error, refetch } = useAPI('/api/v1/databases')
  const sep = dbParam ? '&' : '?'
  const findings = useAPI(`/api/v1/findings${dbParam}${sep}limit=5`)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />
  if (!data) return null

  const { summary, databases } = data

  if (summary.total_databases === 0) {
    return <OnboardingWelcome />
  }

  return (
    <div className="space-y-6">
      <HealthHero summary={summary} />

      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        <StatCard label="Databases"
          value={summary.total_databases}
          data-testid="stat-databases" />
        <StatCard label="Healthy"
          value={summary.healthy} color="var(--green)"
          data-testid="stat-healthy" />
        <StatCard label="Degraded" value={summary.degraded}
          color={summary.degraded > 0
            ? 'var(--red)' : 'var(--green)'} />
        <StatCard label="Critical Findings"
          value={summary.total_critical}
          color={summary.total_critical > 0
            ? 'var(--red)' : 'var(--text-primary)'} />
      </div>

      <div className="rounded p-4"
        data-testid="db-list"
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
              data-testid="db-list-item"
              className="flex items-center gap-3 p-2 rounded"
              style={{ background: 'var(--bg-primary)' }}>
              <StatusDot connected={db.status.connected}
                error={db.status.error} />
              <StatusLabel connected={db.status.connected}
                error={db.status.error} />
              <span className="font-medium flex-1">{db.name}</span>
              <span className="text-xs px-2 py-0.5 rounded"
                style={{
                  background: 'var(--bg-hover)',
                  color: 'var(--text-secondary)',
                }}>
                Score: {db.status.health_score}
              </span>
              {db.trust_level && (
                <span className="text-xs px-2 py-0.5 rounded"
                  data-testid="trust-level-badge"
                  style={{
                    background: 'var(--bg-hover)',
                    color: 'var(--text-secondary)',
                  }}>
                  Trust: {db.trust_level}
                </span>
              )}
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
          data-testid="recent-findings"
          style={{
            background: 'var(--bg-card)',
            border: '1px solid var(--border)',
          }}>
          <h2 className="text-sm font-medium mb-3"
            style={{ color: 'var(--text-secondary)' }}>
            Recent Recommendations
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
