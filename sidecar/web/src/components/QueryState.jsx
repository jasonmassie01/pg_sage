import { RefreshCw } from 'lucide-react'
import { ErrorBanner } from './ErrorBanner'
import { LoadingSpinner } from './LoadingSpinner'
import { clockTime } from '../lib/clock'
import { queryBlocked, queryStale } from '../lib/queryState'

// QueryState renders a useAPI result: the skeleton while the first load
// runs, the server's message with Retry when it failed with no data, and
// otherwise the children, with a strip above them when a refresh failed
// so a transient 502 never unmounts what the operator is reading.
// children may be a function, called only once the data can be shown.
// skeleton null renders nothing while loading. message words the error;
// failure replaces the banner for pages with their own error state.
export function QueryState({ query, skeleton, message, failure, children }) {
  if (queryBlocked(query)) {
    if (query.loading) return skeleton === undefined ? <LoadingSpinner /> : skeleton
    return failure ?? <ErrorBanner message={message ? message(query.error) : query.error}
      onRetry={query.refetch} />
  }
  return (
    <>
      {queryStale(query) ? <StaleStrip query={query} message={message} /> : null}
      {typeof children === 'function' ? children(query.data) : children}
    </>
  )
}

// StaleStrip says the refresh failed, when, why, and how old the data
// shown is. It is a polite status, not an alert: the data is still usable.
export function StaleStrip({ query, message }) {
  const failed = query.failedAt ? ` at ${clockTime(query.failedAt)}` : ''
  const shown = query.updatedAt
    ? `Showing data from ${clockTime(query.updatedAt)}`
    : 'Showing the last loaded data'
  const reason = message ? message(query.error) : query.error
  return (
    <div role="status" data-testid="query-stale"
      className="mb-3 flex items-center gap-2 rounded px-3 py-1.5 text-xs"
      style={{ border: '1px solid var(--yellow)', color: 'var(--text-secondary)' }}>
      <span className="flex-1">Refresh failed{failed} (<span>{reason}</span>). {shown}</span>
      {query.refetch && (
        <button type="button" onClick={() => query.refetch()} aria-label="Retry"
          className="flex items-center gap-1 underline" style={{ color: 'var(--accent)' }}>
          <RefreshCw size={12} aria-hidden="true" /> Retry
        </button>
      )}
    </div>
  )
}
