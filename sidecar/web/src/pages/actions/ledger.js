// Ledger rows come from two id spaces. Only rows the server marks as
// executed (sage.action_log) can be rolled back (G9-B01).
export function isQueuedRow(row) {
  return row.record_kind === 'queued'
}

export function canRollBackRow(row) {
  return row.record_kind === 'executed' && Boolean(row.rollback_sql)
    && ['success', 'monitoring', 'pending'].includes(row.outcome)
}

export const queuedLabels = {
  pending: 'Pending approval',
  approved: 'Approved',
  failed: 'Failed',
  expired: 'Expired',
  rejected: 'Rejected',
}
