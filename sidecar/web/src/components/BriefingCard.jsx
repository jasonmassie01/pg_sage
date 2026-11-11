import { useAPI } from '../hooks/useAPI'
import { ErrorBanner } from './ErrorBanner'
import { QueryState } from './QueryState'
import { TimeAgo } from './TimeAgo'

// BriefingCard shows the latest stored health briefing of each selected
// database (llm-06b), read from GET /api/v1/briefings/latest. A 404 is
// "no briefing yet" with the server's reason; other failures are errors.

const SEVERITY_ORDER = ['critical', 'warning', 'info']

const cardStyle = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

const mutedStyle = { color: 'var(--text-secondary)' }

function severityParts(counts) {
  if (!counts || typeof counts !== 'object') return []
  const known = SEVERITY_ORDER.filter(s => counts[s] > 0)
  const other = Object.keys(counts).filter(s => !SEVERITY_ORDER.includes(s) && counts[s] > 0)
  return [...known, ...other.sort()].map(s => `${counts[s]} ${s}`)
}

function deliveryParts(status) {
  if (!status || typeof status !== 'object' || Object.keys(status).length === 0) {
    return ['not dispatched']
  }
  return Object.keys(status).sort().map(channel => {
    const d = status[channel] || {}
    const code = d.http_status ? ` (HTTP ${d.http_status})` : ''
    return `${channel}: ${d.status || 'unknown'}${code}`
  })
}

function BriefingRow({ briefing }) {
  const db = briefing.database
  const content = briefing.content_json || {}
  const severities = severityParts(content.open_by_severity)
  return (
    <div data-testid={`briefing-${db}`} className="p-3 rounded space-y-1"
      style={{ background: 'var(--bg-primary)' }}>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium">{db}</span>
        <TimeAgo timestamp={briefing.generated_at} />
        <span style={mutedStyle}>
          {briefing.llm_used ? 'LLM summary' : 'Structured'}
        </span>
      </div>
      <div className="text-sm">
        {`${content.total_open ?? 0} open`}
        {severities.length > 0 && ` (${severities.join(', ')})`}
      </div>
      <div className="text-xs" data-testid={`briefing-delivery-${db}`} style={mutedStyle}>
        {deliveryParts(briefing.delivery_status).join(' · ')}
      </div>
      <details>
        <summary className="text-xs cursor-pointer" style={{ color: 'var(--accent)' }}>
          Read the briefing
        </summary>
        <pre data-testid={`briefing-text-${db}`}
          className="text-xs whitespace-pre-wrap mt-2">{briefing.content_text}</pre>
      </details>
    </div>
  )
}

function NoBriefingYet({ reason }) {
  return (
    <div data-testid="briefing-empty" className="text-sm" style={mutedStyle}>
      <div className="font-medium">No briefing yet</div>
      {reason && <div className="text-xs mt-1">{reason}</div>}
    </div>
  )
}

function NamedList({ testId, label, names }) {
  if (!Array.isArray(names) || names.length === 0) return null
  return (
    <div data-testid={testId} className="text-xs" style={mutedStyle}>
      {`${label}: ${names.join(', ')}`}
    </div>
  )
}

function stripStatus(message) {
  return typeof message === 'string' ? message.replace(/ \(404\)$/, '') : ''
}

const loading = (
  <div className="h-12 rounded animate-pulse" data-testid="briefing-loading"
    style={{ background: 'var(--bg-hover)' }} />
)

function BriefingList({ data }) {
  if (!data) return loading
  const briefings = Array.isArray(data.briefings) ? data.briefings : []
  return (
    <div className="space-y-2">
      {briefings.length === 0 && <NoBriefingYet />}
      {briefings.map(b => <BriefingRow key={b.database} briefing={b} />)}
      <NamedList testId="briefing-missing" label="No briefing yet"
        names={data.without_briefing} />
      <NamedList testId="briefing-unavailable" label="Unavailable"
        names={data.unavailable} />
    </div>
  )
}

// BriefingBody keeps the last briefing under a failed refresh (with the
// stale strip), like every page (dashboard-01).
function BriefingBody({ query }) {
  const failure = query.errorStatus === 404
    ? <NoBriefingYet reason={stripStatus(query.error)} />
    : (
      <div data-testid="briefing-error">
        <ErrorBanner message={query.error} onRetry={query.refetch} />
      </div>
    )
  return (
    <QueryState query={query} skeleton={loading} failure={failure}>
      {data => <BriefingList data={data} />}
    </QueryState>
  )
}

export function BriefingCard({ database }) {
  const param = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  const query = useAPI(`/api/v1/briefings/latest${param}`, 60000)
  return (
    <div className="rounded p-4" data-testid="briefing-card" style={cardStyle}>
      <h2 className="text-sm font-medium mb-3" style={mutedStyle}>
        Latest briefing
      </h2>
      <BriefingBody query={query} />
    </div>
  )
}
