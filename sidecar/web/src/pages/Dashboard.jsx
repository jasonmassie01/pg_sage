/* eslint-disable react-refresh/only-export-components */
import { useEffect, useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { useLiveRefetch } from '../hooks/useLiveEvents'
import { SeverityBadge } from '../components/SeverityBadge'
import { ErrorBanner } from '../components/ErrorBanner'
import { TokenBudgetBanner } from '../components/TokenBudgetBanner'
import { FleetHealthChart } from '../components/FleetHealthChart'
import { DatabaseTile } from '../components/DatabaseTile'
import {
  CheckCircle, Clock, ListChecks, Server,
} from 'lucide-react'

const TRUST_LEVEL_LABELS = {
  0: 'Observation',
  1: 'Advisory',
  2: 'Autonomous',
  observation: 'Observation',
  advisory: 'Advisory',
  autonomous: 'Autonomous',
}

export function formatTrustLevel(raw) {
  if (raw === null || raw === undefined || raw === '') return null
  const key = typeof raw === 'number' ? raw : String(raw).toLowerCase()
  return TRUST_LEVEL_LABELS[key] || String(raw)
}

function StatCard({
  label, value, color, 'data-testid': testId, badge,
}) {
  const isLoading = value === undefined || value === null
  return (
    <div className="rounded p-4 relative"
      data-testid={testId}
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div className="text-xs mb-1"
        style={{ color: 'var(--text-secondary)' }}>{label}</div>
      {isLoading ? (
        <div className="h-7 w-16 rounded animate-pulse"
          data-testid="stat-card-skeleton"
          style={{ background: 'var(--bg-hover)' }} />
      ) : (
        <div className="text-2xl font-bold"
          style={{ color: color || 'var(--text-primary)' }}>
          {value}
        </div>
      )}
      {badge !== undefined && badge !== null && badge > 0 && (
        <span
          data-testid="new-since-visit-badge"
          className="absolute top-2 right-2 px-1.5 py-0.5 rounded-full
            text-xs font-semibold"
          style={{ background: 'var(--red)', color: '#fff' }}>
          +{badge} new
        </span>
      )}
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

export function Dashboard({ database, onSelectDB }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const { data, loading, error, refetch } = useAPI('/api/v1/databases')
  const sep = dbParam ? '&' : '?'
  const findings = useAPI(`/api/v1/findings${dbParam}${sep}limit=5`)
  useLiveRefetch(['findings', 'health'], refetch)
  useLiveRefetch(['findings'], findings.refetch)

  const [lastVisitAt, setLastVisitAt] = useState(() => {
    try {
      const v = localStorage.getItem('pg_sage_findings_visit')
      return v ? parseInt(v, 10) : 0
    } catch {
      return 0
    }
  })

  useEffect(() => {
    const onStorage = e => {
      if (e.key === 'pg_sage_findings_visit') {
        setLastVisitAt(parseInt(e.newValue, 10) || 0)
      }
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  const newSinceVisit = (() => {
    const list = findings.data?.findings
    if (!Array.isArray(list) || !lastVisitAt) return 0
    return list.filter(f => {
      const t = f.first_seen || f.created_at || f.last_seen
      if (!t) return false
      return new Date(t).getTime() > lastVisitAt
    }).length
  })()

  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const summary = data?.summary
  const databases = data?.databases

  if (!loading && (!summary || summary.total_databases === 0)) {
    return <OnboardingWelcome />
  }

  return (
    <div className="space-y-6">
      <TokenBudgetBanner />
      {summary && <HealthHero summary={summary} />}

      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        <StatCard label="Databases"
          value={summary?.total_databases}
          data-testid="stat-databases" />
        <StatCard label="Healthy"
          value={summary?.healthy} color="var(--green)"
          data-testid="stat-healthy" />
        <StatCard label="Degraded" value={summary?.degraded}
          color={summary?.degraded > 0
            ? 'var(--red)' : 'var(--green)'} />
        <StatCard label="Critical Findings"
          value={summary?.total_critical}
          color={summary?.total_critical > 0
            ? 'var(--red)' : 'var(--text-primary)'}
          badge={newSinceVisit} />
      </div>

      <FleetHealthChart database={database} />

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
        {loading && !databases && (
          <div className="h-24 rounded animate-pulse"
            data-testid="db-list-skeleton"
            style={{ background: 'var(--bg-hover)' }} />
        )}
        <div
          className="grid gap-3"
          data-testid="db-tile-grid"
          style={{
            gridTemplateColumns:
              'repeat(auto-fill, minmax(220px, 1fr))',
          }}>
          {(databases || []).map(db => (
            <DatabaseTile
              key={db.name}
              db={db}
              selected={database === db.name}
              onSelect={onSelectDB}
            />
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
            {findings.data.findings.slice(0, 5).map((f, i) => (
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
          <a href="#/findings" className="inline-block mt-3 text-sm"
            style={{ color: 'var(--accent)' }}>
            View all recommendations
          </a>
        </div>
      )}
    </div>
  )
}
