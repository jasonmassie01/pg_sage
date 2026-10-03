// LoadMore asks for the next page of a cursor-paged list (useCursorPages)
// and shows why the last request failed.
export function LoadMore({ paging, testId }) {
  if (!paging.hasMore && !paging.error) return null
  return (
    <div className="flex items-center gap-3">
      {paging.hasMore && (
        <button onClick={paging.loadMore} disabled={paging.loading}
          data-testid={testId}
          className="px-3 py-1.5 rounded text-sm"
          style={{
            background: 'var(--bg-card)',
            color: 'var(--text-primary)',
            border: '1px solid var(--border)',
          }}>
          {paging.loading ? 'Loading…' : 'Load more'}
        </button>
      )}
      {paging.error && (
        <span className="text-xs" style={{ color: 'var(--red)' }}>
          {paging.error}
        </span>
      )}
    </div>
  )
}
