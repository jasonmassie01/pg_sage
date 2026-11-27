import { useAPI } from '../../hooks/useAPI'

// Databricks runtime health from the admin deployment status
// (databricks-06): Lakebase credential expiry and mint failures, the latest
// AI Gateway authentication outcomes, and per-stream telemetry lag,
// backlog and warehouse statements per hour. It renders nothing outside
// Databricks, while the status loads, or when the status is unavailable.

const muted = { color: 'var(--text-secondary)' }
const warn = { color: 'var(--yellow)' }
const DASH = '—'

function since(iso) {
  return (Date.now() - new Date(iso).getTime()) / 1000
}

function humanSeconds(seconds) {
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s} s`
  if (s < 7200) return `${Math.round(s / 60)} min`
  if (s < 172800) return `${Math.round(s / 3600)} h`
  return `${Math.round(s / 86400)} d`
}

function statementsPerHour(stream) {
  if (!stream?.counters_since) return DASH
  const hours = since(stream.counters_since) / 3600
  if (!(hours >= 1 / 60)) return DASH
  return `${Math.round((stream.statements || 0) / hours)}/h`
}

function endpointLabel(path) {
  const parts = String(path || '').split('/')
  return parts.length >= 2 ? parts.slice(-2).join('/') : String(path || '')
}

function Alert({ children }) {
  return <div role="alert" className="text-xs" style={warn}>{children}</div>
}

function Endpoint({ ep }) {
  const left = ep.expires_at ? -since(ep.expires_at) : null
  const failures = ep.consecutive_mint_failures || 0
  return (
    <li className="space-y-0.5">
      <div className="text-sm">
        <span style={{ color: 'var(--text-primary)' }}>{endpointLabel(ep.endpoint)}</span>{' '}
        <span className="text-xs" style={muted}>
          {left === null ? 'no credential minted yet'
            : left > 0 ? `expires in ${humanSeconds(left)}` : 'credential expired'}
        </span>
      </div>
      {left !== null && left <= 0 && (
        <Alert>The cached database credential has expired.</Alert>
      )}
      {failures > 0 && (
        <Alert>
          {failures} consecutive mint failures
          {ep.last_error_class ? ` (${ep.last_error_class})` : ''}
        </Alert>
      )}
    </li>
  )
}

function Credentials({ endpoints }) {
  if (!endpoints || endpoints.length === 0) {
    return <div className="text-xs" style={muted}>No Lakebase OAuth endpoint.</div>
  }
  return (
    <ul className="space-y-2">
      {endpoints.map(ep => <Endpoint key={ep.endpoint} ep={ep} />)}
    </ul>
  )
}

function Gateway({ gateway }) {
  const g = gateway || {}
  if (!g.last_auth_success_at && !g.last_auth_failure_at) {
    return <div className="text-xs" style={muted}>AI Gateway: no Gateway request yet.</div>
  }
  if (!g.authentication_observed && g.last_auth_failure_at) {
    return (
      <Alert>
        Gateway authentication failing ({g.last_failure_class || 'unknown'}),
        last failure {humanSeconds(since(g.last_auth_failure_at))} ago
        {g.last_auth_success_at
          ? `; last success ${humanSeconds(since(g.last_auth_success_at))} ago` : ''}
      </Alert>
    )
  }
  return (
    <div className="text-xs" style={muted}>
      AI Gateway: last success {humanSeconds(since(g.last_auth_success_at))} ago
      {g.last_auth_failure_at
        ? `; last failure ${humanSeconds(since(g.last_auth_failure_at))} ago` : ''}
    </div>
  )
}

function Backlog({ delivery }) {
  if (!delivery || delivery.length === 0) return null
  return (
    <ul className="text-xs" style={muted}>
      {delivery.map(d => (
        <li key={d.consumer}>
          {d.consumer}: {d.pending_rows}{d.pending_rows_capped ? '+' : ''} pending
          {d.oldest_pending_age_seconds != null
            ? `, oldest ${humanSeconds(d.oldest_pending_age_seconds)}` : ''}
        </li>
      ))}
    </ul>
  )
}

function Binding({ name, row }) {
  const streams = Array.isArray(row?.streams) ? row.streams : []
  return (
    <div className="space-y-1">
      <div className="text-sm">
        <span style={{ color: 'var(--text-primary)' }}>{name}</span>{' '}
        <span className="text-xs" style={muted}>{row?.state}</span>
      </div>
      <table className="text-xs w-full">
        <thead style={muted}>
          <tr><th scope="col" align="left">Stream</th><th scope="col" align="left">Lag</th>
            <th scope="col" align="left">Statements</th>
            <th scope="col" align="left">Last error</th></tr>
        </thead>
        <tbody>
          {streams.map(s => (
            <tr key={s.stream}>
              <td>{s.stream}</td>
              <td>{s.forward_lag_seconds != null ? humanSeconds(s.forward_lag_seconds) : DASH}</td>
              <td>{statementsPerHour(s)}</td>
              <td>{s.last_error || ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <Backlog delivery={row?.delivery} />
    </div>
  )
}

export function DatabricksStatusCard() {
  const { data } = useAPI('/api/v1/deployment/status', 60000)
  const db = data?.databricks
  if (!db?.active) return null
  const telemetry = db.telemetry && typeof db.telemetry === 'object' ? db.telemetry : {}
  const bindings = Object.keys(telemetry).sort()
  return (
    <section className="rounded p-5 mb-4 space-y-3"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <h3 className="text-sm font-medium">Databricks runtime</h3>
      <Credentials endpoints={db.endpoints} />
      <Gateway gateway={db.gateway} />
      {bindings.map(name => <Binding key={name} name={name} row={telemetry[name]} />)}
    </section>
  )
}
