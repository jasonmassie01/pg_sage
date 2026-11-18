import { useAPI } from '../../hooks/useAPI'
import { GradeChip } from '../../components/review/GradeChip'
import {
  budgetState, connectionLabel, formatRun, haLabel, healthLabel, healthReasons,
  stopLabel, trustState,
} from './runtimeStatus'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }

// DatabaseSummary is the Database page's runtime card (fleet-06): what
// the sidecar reports for one database, then its fleet look-alikes.
export function DatabaseSummary({ database }) {
  const fleet = useAPI('/api/v1/databases', 0)
  const rows = Array.isArray(fleet.data?.databases) ? fleet.data.databases : null
  const runtime = rows?.find(row => row?.name === database)
  return (
    <section className="rounded p-4 space-y-3" style={card} data-testid="db-summary">
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-sm font-medium" style={muted}>Database: {database}</h2>
        <GradeChip database={database} />
      </div>
      {fleet.loading && <p className="text-sm" style={muted}>Loading runtime status...</p>}
      {fleet.error && (
        <p className="text-sm" data-testid="summary-runtime-error" style={{ color: 'var(--red)' }}>
          Runtime status unavailable: {fleet.error}
        </p>
      )}
      {rows && !runtime && (
        <p className="text-sm" data-testid="summary-not-running" style={{ color: 'var(--yellow)' }}>
          {connectionLabel(undefined, true)}: this sidecar reports no runtime for {database}.
        </p>
      )}
      {runtime && <RuntimeFacts runtime={runtime} />}
      <LookAlikes database={database} />
    </section>
  )
}

function Fact({ label, testId, children, color }) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wider" style={muted}>{label}</dt>
      <dd className="text-sm mt-1" data-testid={testId}
        style={{ color: color || 'var(--text-primary)' }}>{children}</dd>
    </div>
  )
}

function RuntimeFacts({ runtime }) {
  const status = runtime.status || {}
  const reasons = healthReasons(status)
  const budget = budgetState(runtime)
  const ha = haLabel(status)
  return (
    <dl className="grid gap-3 sm:grid-cols-3">
      <Fact label="Connection" testId="summary-connection">
        {connectionLabel(runtime)}{ha ? ` · ${ha}` : ''}
      </Fact>
      <Fact label="Platform / Postgres" testId="summary-version">
        {status.platform || 'Postgres'}{status.pg_version ? ` / ${status.pg_version}` : ''}
      </Fact>
      <Fact label="Health" testId="summary-health">
        {healthLabel(status)}{reasons.length > 0 ? ` (${reasons.join('; ')})` : ''}
      </Fact>
      <Fact label="Last collector run" testId="summary-collector">
        {formatRun(status.collector_last_run)}
      </Fact>
      <Fact label="Last analyzer run" testId="summary-analyzer">
        {formatRun(status.analyzer_last_run)}
      </Fact>
      <Fact label="Effective trust" testId="summary-trust">
        {trustState(status).effective}
      </Fact>
      <Fact label="Emergency stop" testId="summary-stop"
        color={runtime.emergency_stopped ? 'var(--red)' : undefined}>
        {stopLabel(runtime)}
      </Fact>
      <Fact label="LLM budget (tokens today)" testId="summary-budget"
        color={budget.exhausted ? 'var(--red)' : undefined}>
        {budget.label}{budget.exhausted ? ' · exhausted' : ''}
      </Fact>
    </dl>
  )
}

// validLookAlikes is the list, [] for a null one, or null when the body
// does not have the documented shape.
function validLookAlikes(data) {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return null
  if (data.lookalikes === null) return []
  if (!Array.isArray(data.lookalikes)) return null
  const ok = data.lookalikes.every(item => typeof item?.database === 'string' &&
    item.database && Number.isFinite(item.similarity))
  return ok ? data.lookalikes : null
}

function LookAlikes({ database }) {
  const query = useAPI(`/api/v1/fleet/lookalikes?database=${encodeURIComponent(database)}`, 0)
  let body
  if (query.loading) {
    body = <p className="text-sm" style={muted}>Loading look-alikes...</p>
  } else if (query.error && query.errorStatus === 503) {
    body = <p className="text-sm" data-testid="lookalikes-off" style={muted}>
      Fleet learning is not running, so look-alikes are unavailable.</p>
  } else if (query.error) {
    body = <LookAlikesError message={query.error} />
  } else {
    const items = validLookAlikes(query.data)
    if (items === null) body = <LookAlikesError message="Invalid look-alikes response" />
    else if (items.length === 0) {
      body = <p className="text-sm" data-testid="lookalikes-none" style={muted}>
        No look-alikes in this database&apos;s fleet-learning boundary.</p>
    } else {
      body = <ul className="text-sm space-y-1" data-testid="lookalikes-list">
        {items.map(item => (
          <li key={item.database} style={{ color: 'var(--text-primary)' }}>
            <span className="font-mono">{item.database}</span>
            <span style={muted}> · {Math.round(item.similarity * 100)}% similar</span>
          </li>
        ))}
      </ul>
    }
  }
  return (
    <div>
      <h3 className="text-xs uppercase tracking-wider mb-1" style={muted}>Look-alikes</h3>
      {body}
    </div>
  )
}

function LookAlikesError({ message }) {
  return (
    <p role="alert" data-testid="lookalikes-error" className="text-sm p-2 rounded"
      style={{ color: 'var(--red)', border: '1px solid var(--red)' }}>
      Could not load look-alikes: {message}
    </p>
  )
}
