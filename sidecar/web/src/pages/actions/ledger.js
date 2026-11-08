// Ledger rows come from two id spaces. Only rows the server marks as
// executed (sage.action_log) can be rolled back (G9-B01).
export function isQueuedRow(row) {
  return row.record_kind === 'queued'
}

// A withheld (rollback_skipped) or failed rollback is retried by the same
// endpoint; the button must show exactly then (executor-07).
const retryableRollbacks = ['rollback_skipped', 'rollback_failed']

// A revert abandoned because the index is no longer pg_sage's must not be
// retried; the backend refuses it too.
const abandonedRevertPrefix = 'revert refused: '

function isAbandonedRevert(row) {
  return row.outcome === 'rollback_skipped'
    && String(row.rollback_reason || '').startsWith(abandonedRevertPrefix)
}

export function canRollBackRow(row) {
  return row.record_kind === 'executed' && Boolean(row.rollback_sql)
    && ['success', 'monitoring', 'pending', ...retryableRollbacks].includes(row.outcome)
    && !isAbandonedRevert(row)
}

export function rollbackButtonLabel(row) {
  return retryableRollbacks.includes(row.outcome) ? 'Retry rollback' : 'Roll Back Action'
}

export function showsRollbackReason(row) {
  return Boolean(row.rollback_reason)
    && ['rolled_back', ...retryableRollbacks].includes(row.outcome)
}

export const queuedLabels = {
  pending: 'Pending approval',
  approved: 'Approved',
  failed: 'Failed',
  expired: 'Expired',
  rejected: 'Rejected',
}
