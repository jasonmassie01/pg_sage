import { useAPI } from '../../hooks/useAPI'

// Fleet canaries on the Earned autonomy page (executor-04, dashboard-14):
// each run of a validated change across the fleet, and, while a run is
// still changing databases, each database's stored state in it from
// GET /api/v1/sre/autonomy/rollouts/{id}. A missing state reads "unknown",
// never blank.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

// ACTIVE_STATES are the run states (sage.rollout_run.state) in which the
// canary is still changing or measuring databases.
const ACTIVE_STATES = new Set(['canary', 'expanding'])
// A running canary moves when one database's verdict lands; its detail is
// read more often than the list.
const DETAIL_POLL_MS = 15000

function RolloutInstances({ id }) {
  const detail = useAPI(`/api/v1/sre/autonomy/rollouts/${encodeURIComponent(id)}`,
    DETAIL_POLL_MS)
  if (detail.error && !detail.data) {
    return <div data-testid="rollout-instances-error" style={{ color: 'var(--red)' }}>
      Per-database state could not be read: {detail.error}</div>
  }
  if (!detail.data) return null
  const instances = Array.isArray(detail.data.instances) ? detail.data.instances : []
  if (instances.length === 0) {
    return <div data-testid="rollout-instances-unknown">
      Databases: unknown (no database has reported a state yet)</div>
  }
  return (
    <ul data-testid="rollout-instances" className="ml-4 list-disc">
      {instances.map(inst => (
        <li key={`${inst.ordinal}:${inst.database}`} data-testid="rollout-instance">
          {inst.database || 'unknown'}: {inst.status || 'unknown'}
        </li>
      ))}
    </ul>
  )
}

export function Rollouts({ data }) {
  const items = data?.items || []
  return (
    <div className="rounded border p-3 text-xs" style={card}>
      <h3 className="text-sm font-semibold" style={strong}>Fleet canaries</h3>
      {items.length === 0 && <div style={muted}>No canary rollouts.</div>}
      <ul style={muted}>
        {items.map(r => (
          <li key={r.id} data-testid="rollout-run">
            {r.created_at} {r.source_database}: {r.state || 'unknown'}
            {r.halt_reason ? ` (${r.halt_reason})` : ''}, {r.applied_instances} applied
            {ACTIVE_STATES.has(r.state) && <RolloutInstances id={r.id} />}
          </li>
        ))}
      </ul>
    </div>
  )
}
