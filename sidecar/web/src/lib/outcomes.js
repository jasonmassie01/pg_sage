// outcomes.js: the outcome ledger's verdicts and rollback states in words
// (ux-02a), shared by the Ledger page and the Inbox (ux-01b).

export const VERDICT_LABELS = Object.freeze({
  pending: 'Not judged yet', improved: 'Improved', no_change: 'No change',
  regressed: 'Regressed', inconclusive: 'Inconclusive', unverifiable: 'Cannot be verified',
  expired: 'Expired before a verdict',
})

export const ROLLBACK_LABELS = Object.freeze({
  available: 'Can roll back', rolled_back: 'Rolled back', rollback_failed: 'Rollback failed',
  not_reversible: 'Not reversible', rolling_back: 'Rolling back',
})
