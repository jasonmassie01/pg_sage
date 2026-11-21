import { useAPI } from '../hooks/useAPI'

// QueryCallSite (pr-01a): the line of application code that sends the
// statement, read from the configured code host. It is fetched apart from
// the query page, so a slow code host never delays the page, and refreshed
// rarely because each lookup reads the repository.

const muted = { color: 'var(--text-secondary)' }
const REFRESH_MS = 10 * 60 * 1000

export function QueryCallSite({ queryid, database }) {
  const { data, loading, error } = useAPI(
    `/api/v1/queries/${queryid}/call-site?database=${encodeURIComponent(database)}`,
    REFRESH_MS)
  if (error) {
    return <p data-testid="query-call-site-error" className="text-xs"
      style={{ color: 'var(--red)' }}>Call site lookup failed: {error}</p>
  }
  if (!data) {
    return loading ? <p className="text-xs" style={muted}>Looking up the call site…</p> : null
  }
  if (!data.configured) {
    return <p data-testid="query-call-site-off" className="text-xs" style={muted}>
      Call site: {data.note}</p>
  }
  const site = data.call_site || {}
  const several = !site.path && site.candidates?.length > 1
  return (
    <div data-testid="query-call-site" className="text-xs mt-2 space-y-1">
      <p style={{ color: 'var(--text-primary)' }}>{data.summary}</p>
      {several && (
        <ul className="list-disc pl-4">
          {site.candidates.map(c => (
            <li key={`${c.path}:${c.line}`} data-testid="call-site-candidate">
              {c.path}:{c.line}</li>
          ))}
        </ul>
      )}
      <p style={muted}>In {data.repo} via {data.host}, by {site.method}.</p>
    </div>
  )
}
