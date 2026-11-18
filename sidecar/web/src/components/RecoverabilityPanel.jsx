import { useAPI } from '../hooks/useAPI'
import { QueryState } from './QueryState'
import { LoadingSpinner } from './LoadingSpinner'
import { TimeAgo } from './TimeAgo'
import { formatDuration } from '../lib/recoverability'

// drill-01d: whether a database can be restored, from its restore drills
// (drill-01a) and the backup isolation evidence (drill-01b). The database
// page shows the panel; the fleet overview shows one line with the spec's
// number, databases with a passed drill in the last 35 days. Read-only:
// nothing here starts a drill.

const STATUS = {
  passed: { label: 'Passed', color: 'var(--green)' },
  failed: { label: 'Failed', color: 'var(--red)' },
  stale: { label: 'Stale', color: 'var(--yellow)' },
  never_drilled: { label: 'Never drilled', color: 'var(--yellow)' },
  unknown: { label: 'Unknown', color: 'var(--text-secondary)' },
}

const muted = { color: 'var(--text-secondary)' }

function historyURL(database) {
  if (!database || database === 'all') return null
  return `/api/v1/databases/${encodeURIComponent(database)}/restore-drills?limit=20`
}

function LastDrill({ drill }) {
  if (!drill) return null
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
      <span data-testid="recoverability-last">
        Last drill <TimeAgo timestamp={drill.finished_at} />
        {' '}({drill.passed ? 'passed' : 'failed'}, {drill.kind || 'unknown kind'} on
        {' '}{drill.provider || 'unknown provider'})
      </span>
      <span data-testid="recoverability-rpo">
        RPO {formatDuration(drill.rpo_seconds)}
        {drill.rpo_seconds == null && drill.rpo_reason
          ? <span style={muted}> ({drill.rpo_reason})</span> : null}
      </span>
      <span data-testid="recoverability-rto">RTO {formatDuration(drill.rto_seconds)}</span>
    </div>
  )
}

function IsolationLine({ isolation }) {
  let text = 'Unknown whether backups survive a delete of the database'
  if (isolation?.survives_delete === true) {
    text = 'Backups survive a delete of the database'
  } else if (isolation?.survives_delete === false) {
    text = 'Backups are deleted with the database'
  }
  const detail = isolation?.evidence || isolation?.unavailable
  return (
    <div data-testid="recoverability-isolation" className="text-xs" style={muted}>
      {text}{detail ? `: ${detail}` : ' (no cloud backup evidence)'}
    </div>
  )
}

function StatusReason({ data }) {
  if (data.status === 'unknown') {
    return (
      <div data-testid="recoverability-unknown" className="text-sm" style={muted}>
        Unknown: {data.reason || 'no restore drill can run'}.
        {data.advice ? ` ${data.advice}` : ''}
      </div>
    )
  }
  if (!data.reason || data.status === 'failed') return null
  return <div className="text-sm" style={muted}>{data.reason}</div>
}

function PanelBody({ data }) {
  const status = STATUS[data?.status] || STATUS.unknown
  const latest = data?.latest
  const shown = data?.last_pass && data?.status === 'passed' ? data.last_pass : latest
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <span data-testid="recoverability-status" className="text-sm font-medium"
          style={{ color: status.color }}>{status.label}</span>
      </div>
      {latest && !latest.passed && (
        <div role="alert" data-testid="recoverability-failure" className="text-sm rounded p-2"
          style={{ border: '1px solid var(--red)', color: 'var(--red)' }}>
          The newest restore drill failed: {latest.failure || 'no failure recorded'}
        </div>
      )}
      <StatusReason data={data || {}} />
      <LastDrill drill={shown} />
      <IsolationLine isolation={data?.isolation} />
      {data?.next_full_copy_cost_label && (
        <div data-testid="recoverability-cost" className="text-xs" style={muted}>
          Next full-copy drill: {data.next_full_copy_cost_label}
        </div>
      )}
    </div>
  )
}

// RecoverabilityPanel is the database page's card for one database.
export function RecoverabilityPanel({ database }) {
  const query = useAPI(historyURL(database), 60000)
  if (!historyURL(database)) return null
  return (
    <section data-testid="recoverability" className="rounded p-4 space-y-3"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <h2 className="text-sm font-medium">Recoverability</h2>
      <QueryState query={query} skeleton={<LoadingSpinner />}>
        {data => <PanelBody data={data} />}
      </QueryState>
    </section>
  )
}

// FleetRecoverabilityLine is the fleet overview's one line; it renders
// nothing until a well-formed count arrives.
export function FleetRecoverabilityLine() {
  const { data } = useAPI('/api/v1/fleet/recoverability', 60000)
  if (!Number.isInteger(data?.total) || !Number.isInteger(data?.passed)) return null
  const noun = data.total === 1 ? 'database' : 'databases'
  const days = Number.isInteger(data.window_days) ? data.window_days : 35
  const median = typeof data.median_rto_seconds === 'number'
    ? `; median RTO ${formatDuration(data.median_rto_seconds)}` : ''
  return (
    <div data-testid="fleet-recoverability" className="text-sm" style={muted}>
      {data.passed} of {data.total} {noun} passed a restore drill in the last {days} days
      {median}
    </div>
  )
}
