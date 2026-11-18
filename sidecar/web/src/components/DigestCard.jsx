import { useAPI } from '../hooks/useAPI'
import { ErrorBanner } from './ErrorBanner'
import { QueryState } from './QueryState'
import { TimeAgo } from './TimeAgo'

// DigestCard shows each selected database's latest digest (ux-02b) on
// home, read from GET /api/v1/briefings/latest: the headline, then Did,
// Verified or reverted, Saved, Needs you and Could not do and why, in the
// order the server stored them, and how each channel took it. Every count
// is the server's; the card does no arithmetic. A 404 is "no digest yet"
// with the server's reason; other failures are errors.

const cardStyle = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

const mutedStyle = { color: 'var(--text-secondary)' }

const SUPPRESSED = 'suppressed_unchanged'

function deliveryParts(status) {
  if (!status || typeof status !== 'object' || Object.keys(status).length === 0) {
    return ['not sent yet']
  }
  if (status.digest?.status === SUPPRESSED) {
    return ['Not sent: nothing changed since the last digest']
  }
  return Object.keys(status).sort().map(channel => {
    const d = status[channel] || {}
    const why = d.error ? ` (${d.error})` : ''
    return `${channel}: ${d.status || 'unknown'}${why}`
  })
}

function itemText(item) {
  const count = item.count ? ` (${item.count})` : ''
  const detail = item.detail ? `: ${item.detail}` : ''
  return `${item.label}${count}${detail}`
}

function DigestSection({ section, db }) {
  const items = Array.isArray(section.items) ? section.items : []
  return (
    <div data-testid={`digest-section-${section.key}-${db}`} className="text-sm">
      <span className="font-medium">{section.title}</span>
      <span style={mutedStyle}>{` · ${section.summary}`}</span>
      {items.length > 0 && (
        <ul className="text-xs list-disc ml-5 mt-1" style={mutedStyle}>
          {items.map((item, i) => <li key={`${item.label}-${i}`}>{itemText(item)}</li>)}
        </ul>
      )}
    </div>
  )
}

function DigestBody({ digest, db }) {
  const sections = Array.isArray(digest.sections) ? digest.sections : []
  const gaps = Array.isArray(digest.gaps) ? digest.gaps : []
  return (
    <>
      <div className="text-sm font-medium" data-testid={`digest-headline-${db}`}>
        {digest.headline}
      </div>
      {sections.map(s => <DigestSection key={s.key} section={s} db={db} />)}
      {gaps.length > 0 && (
        <div className="text-xs" data-testid={`digest-gaps-${db}`}
          style={{ color: 'var(--warning, var(--text-secondary))' }}>
          {`Not read: ${gaps.join('; ')}`}
        </div>
      )}
    </>
  )
}

function DigestRow({ briefing }) {
  const db = briefing.database
  const digest = briefing.content_json?.digest
  return (
    <div data-testid={`digest-${db}`} className="p-3 rounded space-y-2"
      style={{ background: 'var(--bg-primary)' }}>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium">{db}</span>
        <TimeAgo timestamp={briefing.generated_at} />
        {digest && (
          <span data-testid={`digest-source-${db}`} style={mutedStyle}>
            {digest.headline_source === 'model' ? 'Model headline' : 'Computed headline'}
          </span>
        )}
      </div>
      {digest
        ? <DigestBody digest={digest} db={db} />
        : (
          <div data-testid={`digest-none-${db}`} className="text-sm" style={mutedStyle}>
            No digest in this briefing: it was stored before digests existed.
          </div>
        )}
      <div className="text-xs" data-testid={`digest-delivery-${db}`} style={mutedStyle}>
        {deliveryParts(briefing.delivery_status).join(' · ')}
      </div>
    </div>
  )
}

function NoDigestYet({ reason }) {
  return (
    <div data-testid="digest-empty" className="text-sm" style={mutedStyle}>
      <div className="font-medium">No digest yet</div>
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
  <div className="h-12 rounded animate-pulse" data-testid="digest-loading"
    style={{ background: 'var(--bg-hover)' }} />
)

function DigestList({ data }) {
  if (!data) return loading
  const briefings = Array.isArray(data.briefings) ? data.briefings : []
  return (
    <div className="space-y-2">
      {briefings.length === 0 && <NoDigestYet />}
      {briefings.map(b => <DigestRow key={b.database} briefing={b} />)}
      <NamedList testId="digest-missing" label="No digest yet"
        names={data.without_briefing} />
      <NamedList testId="digest-unavailable" label="Unavailable"
        names={data.unavailable} />
    </div>
  )
}

// DigestState keeps the last digest under a failed refresh (with the
// stale strip), like every page (dashboard-01).
function DigestState({ query }) {
  const failure = query.errorStatus === 404
    ? <NoDigestYet reason={stripStatus(query.error)} />
    : (
      <div data-testid="digest-error">
        <ErrorBanner message={query.error} onRetry={query.refetch} />
      </div>
    )
  return (
    <QueryState query={query} skeleton={loading} failure={failure}>
      {data => <DigestList data={data} />}
    </QueryState>
  )
}

export function DigestCard({ database }) {
  const param = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  const query = useAPI(`/api/v1/briefings/latest${param}`, 60000)
  return (
    <div className="rounded p-4" data-testid="digest-card" style={cardStyle}>
      <h2 className="text-sm font-medium mb-3" style={mutedStyle}>Digest</h2>
      <DigestState query={query} />
    </div>
  )
}
