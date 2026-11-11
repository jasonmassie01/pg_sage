import { useAPI } from '../../hooks/useAPI'
import { QueryState } from '../../components/QueryState'
import { queryBlocked } from '../../lib/queryState'

// Footprint (footprint-08): what pg_sage itself costs each database it
// monitors, per hour against its self-budget, and which component spends
// it. A figure the API reports as null is not measured yet and says so;
// it is never shown as 0. The process CPU is shown once for all databases.

const muted = { color: 'var(--text-secondary)' }
const NOT_MEASURED = 'not measured yet'
const number = new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 })

function footprintURL(database) {
  return database && database !== 'all'
    ? `/api/v1/self-cost?database=${encodeURIComponent(database)}`
    : '/api/v1/self-cost'
}

function measured(value) {
  return value !== null && value !== undefined
}

function amount(value, unit) {
  return measured(value) ? `${number.format(value)} ${unit}` : NOT_MEASURED
}

function bytes(value) {
  if (!measured(value)) return NOT_MEASURED
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = value
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return i === 0 ? `${v} B` : `${v.toFixed(1)} ${units[i]}`
}

// showDerivedSettings scrolls to the Derived settings panel. It is not an
// href="#derived-settings": the dashboard routes on the hash, so that link
// would leave Settings for "Page not found".
function showDerivedSettings() {
  document.getElementById('derived-settings')?.scrollIntoView({ behavior: 'smooth' })
}

function percent(share) {
  return `${Math.round(share * 100)}%`
}

function againstBudget(used, budget, budgetText) {
  if (!measured(budget)) return used
  return `${used} of ${budgetText} budget`
}

function Figure({ testid, label, children }) {
  return (
    <div data-testid={testid} className="text-xs">
      <span style={muted}>{label}: </span>
      <span style={{ color: 'var(--text-primary)' }}>{children}</span>
    </div>
  )
}

function Components({ db }) {
  const rows = Array.isArray(db.by_component) ? db.by_component : []
  if (rows.length === 0) {
    return <div className="text-xs" style={muted}>No component breakdown yet.</div>
  }
  return (
    <div className="text-xs space-y-0.5">
      <div style={muted}>
        By component
        {measured(db.unattributed_share) && ` (${percent(db.unattributed_share)} unattributed:`
          + ' statements pg_stat_statements first saw before their tag named one)'}
      </div>
      {rows.map(c => (
        <div key={c.component} data-testid={`footprint-${db.database}-component-${c.component}`}>
          <span className="font-medium">{c.component}</span>{' '}
          {amount(c.db_time_ms_per_hour, 'ms/h')} · {percent(c.share)} ·{' '}
          {amount(c.calls_per_hour, 'calls/h')} · {amount(c.blocks_per_hour, 'blocks/h')}
        </div>
      ))}
    </div>
  )
}

function TopStatements({ rows }) {
  if (!Array.isArray(rows) || rows.length === 0) return null
  return (
    <details className="text-xs">
      <summary style={muted}>Costliest statements</summary>
      <ul className="space-y-0.5 mt-1">
        {rows.map((s, i) => (
          <li key={`${s.component}-${i}`}>
            <code>{s.text}</code>{' '}
            <span style={muted}>
              {s.component} · {amount(s.db_time_ms, 'ms')} · {number.format(s.calls)} calls
            </span>
          </li>
        ))}
      </ul>
    </details>
  )
}

function DatabaseFootprint({ db }) {
  const id = `footprint-${db.database}`
  const dbTime = amount(db.db_time_ms_per_hour, 'ms/h')
  const blocks = amount(db.blocks_per_hour, 'blocks/h')
  return (
    <div data-testid={id} className="space-y-1 pt-2"
      style={{ borderTop: '1px solid var(--border)' }}>
      <h3 className="text-xs font-semibold">{db.database}</h3>
      <Figure testid={`${id}-db_time`} label="Database time">
        {measured(db.db_time_ms_per_hour)
          ? againstBudget(dbTime, db.db_time_budget_ms_per_hour,
            amount(db.db_time_budget_ms_per_hour, 'ms/h'))
          : NOT_MEASURED}
      </Figure>
      <Figure testid={`${id}-blocks`} label="Shared blocks">
        {measured(db.blocks_per_hour)
          ? againstBudget(blocks, db.blocks_budget_per_hour,
            amount(db.blocks_budget_per_hour, 'blocks/h'))
          : NOT_MEASURED}
      </Figure>
      <Figure testid={`${id}-calls`} label="Statements">
        {amount(db.calls_per_hour, 'calls/h')}
      </Figure>
      <Figure testid={`${id}-rows`} label="Sage-table rows">
        {measured(db.rows_read_per_hour)
          ? `${amount(db.rows_read_per_hour, 'read/h')}, `
            + `${amount(db.rows_written_per_hour, 'written/h')}`
          : NOT_MEASURED}
      </Figure>
      <Figure testid={`${id}-schema`} label="Sage schema">
        {measured(db.schema_bytes)
          ? againstBudget(bytes(db.schema_bytes), db.storage_budget_bytes,
            bytes(db.storage_budget_bytes))
          : NOT_MEASURED}
      </Figure>
      <Figure testid={`${id}-cycle`} label="Collector cycle">
        {amount(db.cycle_seconds, 's')}
      </Figure>
      <Components db={db} />
      <TopStatements rows={db.top_statements} />
    </div>
  )
}

function ProcessCPU({ process }) {
  const loops = Array.isArray(process?.top_loops) ? process.top_loops : []
  const cpu = amount(process?.cpu_ms_per_cycle, 'ms per collector cycle')
  return (
    <Figure testid="footprint-cpu" label="Sidecar CPU (all databases)">
      {measured(process?.cpu_ms_per_cycle)
        ? againstBudget(cpu, process.cpu_budget_ms_per_cycle,
          amount(process.cpu_budget_ms_per_cycle, 'ms'))
        : NOT_MEASURED}
      {loops.length > 0 && ` · busiest loops: ${loops.map(l =>
        `${l.loop} ${number.format(l.busy_ms)} ms (${l.runs} runs)`).join(', ')}`}
    </Figure>
  )
}

export function Footprint({ database }) {
  const url = footprintURL(database)
  const footprint = useAPI(url, 60000)
  const { data } = footprint
  const message = e => `Footprint: ${e}`
  if (queryBlocked(footprint)) {
    return <QueryState query={footprint} skeleton={null} message={message} />
  }
  const databases = Array.isArray(data?.databases) ? data.databases : []
  return (
    <QueryState query={footprint} message={message}>
    <section data-testid="footprint" data-url={url} className="rounded p-4 space-y-2"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
        Footprint
      </h2>
      <p className="text-xs" style={muted}>
        {data?.meaning} The collector cadence and read deadline that follow from this cost
        are under{' '}
        <button type="button" data-testid="footprint-derived-link"
          onClick={showDerivedSettings} className="underline"
          style={{ color: 'var(--accent)' }}>derived settings</button>.
      </p>
      <ProcessCPU process={data?.process} />
      {databases.length === 0 && (
        <div className="text-sm" style={muted}>No databases to show.</div>
      )}
      {databases.map(db => <DatabaseFootprint key={db.database} db={db} />)}
    </section></QueryState>
  )
}
