// A useAPI result ({ data, loading, error, updatedAt, failedAt, refetch })
// is in one of three states for a page: nothing to show yet (loading or
// failed without data), data, or data kept under a failed refresh.

export function hasData(query) {
  return query?.data !== null && query?.data !== undefined
}

// queryBlocked is true when the page can show only a placeholder: the
// first load is running or failed. A failed refresh of kept data never
// blocks the page. (useAPI reports loading only while it has no data.)
export function queryBlocked(query) {
  return Boolean(query?.loading) || (!hasData(query) && Boolean(query?.error))
}

// combineQueries is one query state for a view built from several
// requests (null entries are requests not made): blocked until all have
// data, stale when any refresh failed, with the oldest data time.
export function combineQueries(queries, refetch) {
  const active = queries.filter(Boolean)
  const times = key => active.map(q => q[key]).filter(Number.isFinite)
  const updated = times('updatedAt')
  const failed = times('failedAt')
  return {
    data: active.every(hasData) ? active.map(q => q.data) : null,
    loading: active.some(q => q.loading),
    error: active.find(q => q.error)?.error ?? null,
    updatedAt: updated.length ? Math.min(...updated) : null,
    failedAt: failed.length ? Math.max(...failed) : null,
    refetch,
  }
}

// queryStale is true when a refresh failed and the last good data is shown.
export function queryStale(query) {
  return hasData(query) && Boolean(query?.error)
}
