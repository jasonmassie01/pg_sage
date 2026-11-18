// landingTab picks the tab Actions opens on when no link names one
// (ux-05a win 1): Waiting on you when the role can review and approvals
// wait, else Proposed changes. tab is null while the count is still
// loading; error is the count's read error when the fallback is a guess.
export function landingTab({ canReview = false, pendingCount = 0, loading = false,
  error = null } = {}) {
  if (!canReview) return { tab: 'recommendations', error: null }
  if (Number.isFinite(pendingCount) && pendingCount > 0) return { tab: 'pending', error: null }
  if (loading) return { tab: null, error: null }
  return { tab: 'recommendations', error: error || null }
}

// pendingCountState reads the approval count landingTab needs from the
// Actions queries: the queued rows and managed changes waiting in scope,
// whether either list is still loading, and what failed to load.
export function pendingCountState(q, pendingTotal) {
  const { pending, managed } = q
  const partial = (pending.data?.errors || [])
    .map(e => `${e.database} (${e.error})`).join(', ')
  return {
    pendingCount: pendingTotal,
    loading: Boolean(pending.loading || managed.loading),
    error: pending.error || managed.error
      || (partial ? `could not read the queue of ${partial}` : null),
  }
}
