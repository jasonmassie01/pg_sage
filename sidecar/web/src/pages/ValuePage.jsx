import { useAPI } from '../hooks/useAPI'

const valuePollIntervalMs = 60000

function formatHours(value) {
  const hours = Number(value) || 0
  return `${Number.isInteger(hours) ? hours : hours.toFixed(1)} h`
}

function humanize(value) {
  return String(value || '')
    .replaceAll('_', ' ')
    .replace(/\b\w/g, character => character.toUpperCase())
}

function valueURL(database) {
  if (!database || database === 'all') return '/api/v1/value'
  return `/api/v1/value?database=${encodeURIComponent(database)}`
}

function LoadingState() {
  return (
    <div role="status" aria-label="Loading Value" className="py-12 text-center">
      Loading value delivered...
    </div>
  )
}

function ErrorState({ error, onRetry }) {
  return (
    <div role="alert" className="rounded border p-4"
      style={{ borderColor: 'var(--red)', color: 'var(--text-primary)' }}>
      <p className="font-semibold">Unable to load Value</p>
      <p className="mt-1 text-sm">{error}</p>
      <button type="button" onClick={onRetry}
        className="mt-3 rounded px-3 py-1.5 text-sm"
        style={{ background: 'var(--accent)', color: '#fff' }}>
        Retry
      </button>
    </div>
  )
}

function EmptyState() {
  return (
    <div className="space-y-3">
      <div role="status" className="rounded border p-6 text-center"
        style={{ borderColor: 'var(--border)' }}>
        <p className="font-semibold">No verified savings yet</p>
        <p className="mt-1 text-sm" style={{ color: 'var(--text-secondary)' }}>
          Verified successful actions earn credit once their outcome is proven.
        </p>
      </div>
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        No pending potential savings.
      </p>
    </div>
  )
}

function MetricCard({ label, value }) {
  return (
    <div className="rounded border p-4"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {label}
      </div>
      <div className="mt-1 text-2xl font-semibold"
        style={{ color: 'var(--text-primary)' }}>
        {formatHours(value)}
      </div>
    </div>
  )
}

function RealizedValue({ savings }) {
  return (
    <section aria-label="Realized DBA hours saved" className="space-y-3">
      <h2 className="text-lg font-semibold">Realized DBA hours saved</h2>
      <div className="grid gap-3 md:grid-cols-3">
        <MetricCard label="All time" value={savings.all_time} />
        <MetricCard label="This month" value={savings.this_month} />
        <MetricCard label="This week" value={savings.this_week} />
      </div>
    </section>
  )
}

function PotentialValue({ hours }) {
  return (
    <section aria-label="Potential savings" className="rounded border p-4"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <h2 className="text-lg font-semibold">Potential savings</h2>
      <p className="mt-1 text-2xl font-semibold">{formatHours(hours)}</p>
      <p className="mt-1 text-sm" style={{ color: 'var(--text-secondary)' }}>
        Pending recommendations only. Not included in DBA hours saved.
      </p>
    </section>
  )
}

function BreakdownTable({ label, rows, valueLabel }) {
  return (
    <table aria-label={label} className="w-full text-sm">
      <thead>
        <tr style={{ color: 'var(--text-secondary)' }}>
          <th scope="col" className="py-2 text-left">Name</th>
          <th scope="col" className="py-2 text-right">Hours saved</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(row => (
          <tr key={row.key} style={{ borderTop: '1px solid var(--border)' }}>
            <th scope="row" className="py-2 text-left font-normal">
              {row.label}
            </th>
            <td className="py-2 text-right">{formatHours(row[valueLabel])}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function Breakdowns({ byFeature, byDatabase }) {
  const featureRows = Object.entries(byFeature || {}).map(([name, hours]) => ({
    key: name, label: humanize(name), hours,
  }))
  const databaseRows = (byDatabase || []).map(row => ({
    key: row.name, label: row.name, hours: row.hours,
  }))
  return (
    <div className="grid gap-5 lg:grid-cols-2">
      <div className="rounded border p-4"
        style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
        <BreakdownTable label="Hours saved by feature"
          rows={featureRows} valueLabel="hours" />
      </div>
      <div className="rounded border p-4"
        style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
        <BreakdownTable label="Hours saved by database"
          rows={databaseRows} valueLabel="hours" />
      </div>
    </div>
  )
}

function TrendTable({ rows }) {
  return (
    <div className="rounded border p-4"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <table aria-label="Daily savings trend" className="w-full text-sm">
        <thead>
          <tr style={{ color: 'var(--text-secondary)' }}>
            <th scope="col" className="py-2 text-left">Day</th>
            <th scope="col" className="py-2 text-right">Hours saved</th>
          </tr>
        </thead>
        <tbody>
          {(rows || []).map(row => (
            <tr key={row.day} style={{ borderTop: '1px solid var(--border)' }}>
              <th scope="row" className="py-2 text-left font-normal">
                {row.day}
              </th>
              <td className="py-2 text-right">{formatHours(row.hours)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function IncidentsAvoided({ incidents }) {
  const detail = incidents?.detail || []
  return (
    <section aria-label="Incidents avoided" className="rounded border p-4"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <h2 className="text-lg font-semibold">Incidents avoided</h2>
      <div className="mt-3 flex gap-8">
        <div>
          <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            Count
          </div>
          <div className="text-2xl font-semibold">{incidents?.count || 0}</div>
        </div>
        <div>
          <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            Credited separately
          </div>
          <div className="text-2xl font-semibold">
            {formatHours(incidents?.credited_hours)}
          </div>
        </div>
      </div>
      {detail.length > 0 && (
        <ul className="mt-4 space-y-2">
          {detail.map(item => (
            <li key={item.evidence_id} className="rounded p-3"
              style={{ background: 'var(--bg-primary)' }}>
              <span>{humanize(item.kind)}</span>
              <span className="ml-2 text-xs"
                style={{ color: 'var(--text-secondary)' }}>
                {humanize(item.severity)}
              </span>
              <a className="ml-3 text-sm underline"
                href={`#/ledger?evidence_id=${encodeURIComponent(
                  item.evidence_id,
                )}`}>
                View evidence {item.evidence_id}
              </a>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function isEmptyValue(data) {
  return !data || (
    Number(data.dba_hours_saved?.all_time || 0) === 0 &&
    Number(data.potential_hours_pending || 0) === 0 &&
    Number(data.incidents_avoided?.count || 0) === 0 &&
    Object.keys(data.by_feature || {}).length === 0 &&
    (data.by_database || []).length === 0 &&
    (data.trend_daily || []).length === 0
  )
}

export function ValuePage({ database }) {
  const { data, loading, error, refetch } = useAPI(
    valueURL(database), valuePollIntervalMs,
  )

  if (loading) return <LoadingState />
  if (error) return <ErrorState error={error} onRetry={refetch} />

  return (
    <div className="space-y-6" data-testid="value-page">
      <div>
        <h1 className="text-2xl font-semibold">Value delivered</h1>
        <p className="mt-1 text-sm" style={{ color: 'var(--text-secondary)' }}>
          Verified work completed and incidents prevented by pg_sage.
        </p>
      </div>
      {isEmptyValue(data) ? <EmptyState /> : (
        <>
          <RealizedValue savings={data.dba_hours_saved || {}} />
          <PotentialValue hours={data.potential_hours_pending} />
          <TrendTable rows={data.trend_daily} />
          <Breakdowns byFeature={data.by_feature}
            byDatabase={data.by_database} />
          <IncidentsAvoided incidents={data.incidents_avoided} />
        </>
      )}
    </div>
  )
}
