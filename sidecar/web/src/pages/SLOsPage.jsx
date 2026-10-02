import { Gauge } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'
import { TimeAgo } from '../components/TimeAgo'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

// Sage SRE M5 SLO view: error-budget state per SLO and database, burn
// rates per window, unknown reasons (unknown is never "ok"), and the
// change feed of the selected database.

const STATE_STYLES = {
  ok: { label: 'OK', color: 'var(--green)' },
  ticket: { label: 'Ticket', color: 'var(--yellow)' },
  page: { label: 'Page', color: 'var(--red)' },
  unknown: { label: 'Unknown', color: 'var(--text-secondary)' },
}

const card = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

function humanize(reason) {
  return String(reason || '').replaceAll('_', ' ')
}

function formatBurn(view) {
  if (!view) return '-'
  if (view.burn_rate === null || view.burn_rate === undefined) {
    return `${view.window}: unknown`
  }
  const rounded = Math.round(view.burn_rate * 100) / 100
  return `${view.window}: ${rounded}x`
}

function StateBadge({ state }) {
  const s = STATE_STYLES[state] || STATE_STYLES.unknown
  return (
    <span data-testid="slo-state" className="px-2 py-0.5 rounded text-xs font-medium"
      style={{ color: s.color, border: `1px solid ${s.color}` }}>
      {s.label}
    </span>
  )
}

function RuleCell({ rule }) {
  const color = rule.firing ? 'var(--red)' : 'var(--text-primary)'
  return (
    <div className="text-xs" style={{ color }}>
      <span style={{ color: 'var(--text-secondary)' }}>
        {rule.severity} ≥{rule.factor}x:{' '}
      </span>
      {formatBurn(rule.long)} · {formatBurn(rule.short)}
    </div>
  )
}

function Budget({ value }) {
  const text = value === null || value === undefined
    ? 'unknown' : `${Math.round(value * 100)}%`
  return <span data-testid="slo-budget" className="text-sm">{text}</span>
}

function SLORow({ slo }) {
  const unknown = slo.unknown || []
  return (
    <tr style={{ borderTop: '1px solid var(--border)' }}>
      <td className="p-2 text-sm" style={{ color: 'var(--text-secondary)' }}>
        {slo.database}
      </td>
      <td className="p-2">
        <div className="text-sm font-medium" style={{ color: 'var(--text-primary)' }}>
          {slo.name}
        </div>
        <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          <span>{slo.kind === 'app' ? 'App SLI' : 'DB proxy'}</span>
          {` · target ${slo.target} over ${slo.window}`}
          {slo.description ? ` · ${slo.description}` : ''}
        </div>
      </td>
      <td className="p-2">
        <StateBadge state={slo.state} />
        {slo.customer_impact && (
          <div data-testid="slo-impact" className="text-xs mt-1"
            style={{ color: 'var(--red)' }}>
            Customer impact
          </div>
        )}
        {unknown.length > 0 && (
          <div data-testid="slo-unknown" className="text-xs mt-1"
            style={{ color: 'var(--text-secondary)' }}>
            {unknown.map(humanize).join(', ')}
          </div>
        )}
      </td>
      <td className="p-2">
        {(slo.rules || []).map((r, i) => <RuleCell key={i} rule={r} />)}
      </td>
      <td className="p-2"><Budget value={slo.budget_remaining} /></td>
      <td className="p-2 text-xs"><TimeAgo timestamp={slo.evaluated_at} /></td>
    </tr>
  )
}

function SLOTable({ slos }) {
  return (
    <div className="rounded overflow-x-auto" style={card}>
      <table className="w-full text-left">
        <thead>
          <tr className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            <th className="p-2">Database</th><th className="p-2">SLO</th>
            <th className="p-2">State</th><th className="p-2">Burn rates</th>
            <th className="p-2">Budget left</th><th className="p-2">Evaluated</th>
          </tr>
        </thead>
        <tbody>
          {slos.map(s => <SLORow key={`${s.database}/${s.name}`} slo={s} />)}
        </tbody>
      </table>
    </div>
  )
}

function ChangeFeed({ database }) {
  const { data, error } = useAPI(
    `/api/v1/sre/changes?database=${encodeURIComponent(database)}&window_minutes=1440`,
    60000)
  const changes = data?.changes || []
  return (
    <section data-testid="change-feed" className="mt-6">
      <h3 className="text-sm font-semibold mb-2" style={{ color: 'var(--text-primary)' }}>
        What changed (last 24 h)
      </h3>
      {error && <ErrorBanner message={error} />}
      {!error && changes.length === 0 && (
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          No changes recorded.
        </p>
      )}
      <ul className="space-y-1">
        {changes.map(c => (
          <li key={c.id} className="rounded p-2 text-sm" style={card}>
            <span className="text-xs mr-2" style={{ color: 'var(--accent)' }}>
              {humanize(c.kind)}
            </span>
            <span style={{ color: 'var(--text-primary)' }}>{c.summary}</span>
            <span className="text-xs ml-2" style={{ color: 'var(--text-secondary)' }}>
              {c.source} · {c.signature} · <TimeAgo timestamp={c.occurred_at} />
            </span>
          </li>
        ))}
      </ul>
    </section>
  )
}

export function SLOsPage({ database }) {
  const single = database && database !== 'all'
  const url = single
    ? `/api/v1/sre/slos?database=${encodeURIComponent(database)}`
    : '/api/v1/sre/slos'
  const { data, loading, error } = useAPI(url, 30000)
  if (loading && !data) return <LoadingSpinner />
  const slos = data?.slos || []
  const unavailable = data?.unavailable || []
  return (
    <div>
      {error && <ErrorBanner message={`SLOs unavailable: ${error}`} />}
      {unavailable.length > 0 && (
        <p className="text-xs mb-2" style={{ color: 'var(--yellow)' }}>
          SLO state unavailable for: {unavailable.join(', ')}
        </p>
      )}
      {!error && slos.length === 0 && (
        <div data-testid="slo-empty">
          <EmptyState icon={Gauge} message={'No SLOs evaluated yet. Database proxy ' +
            'SLIs appear after the first evaluation; register app SLIs under sre.slo.'} />
        </div>
      )}
      {slos.length > 0 && <SLOTable slos={slos} />}
      {single && <ChangeFeed database={database} />}
    </div>
  )
}
