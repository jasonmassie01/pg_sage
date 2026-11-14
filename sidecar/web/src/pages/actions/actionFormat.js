import { verdictLabel } from '../../lib/verificationOutcome'

// Presentation helpers for the action ledger and the pending queue
// (moved out of Actions.jsx by dashboard-31; behaviour unchanged).

export function actionStatus(row) {
  return row.status || row.action_status || row.outcome || 'unknown'
}

export function actionRisk(row) {
  return row.risk_tier || row.action_risk || row.risk || 'unknown'
}

export function verificationStatus(row) {
  if (row.verification_outcome) return verdictLabel(row.verification_outcome.verdict)
  return row.verification_status || 'not_started'
}

export function lifecycleStatus(row) {
  return row.lifecycle_state || row.status || 'ready'
}

export function rollbackClassLabel(value) {
  switch (value) {
  case 'no_rollback_needed':
    return 'Rollback: not needed'
  case 'reversible':
    return 'Rollback: reversible'
  case 'forward_fix_only':
    return 'Rollback: forward fix only'
  default:
    return value ? `Rollback: ${value}` : null
  }
}

export function formatActionTime(value) {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return null
  return date.toLocaleString()
}

// actionCaseScope reads the case id from the hash query (#/actions?case=N).
export function actionCaseScope() {
  const query = (window.location.hash || '').split('?')[1] || ''
  return new URLSearchParams(query).get('case') || ''
}

export function actionRowKey(row) {
  // ledger_key (log:N / queue:N) is unique across both id spaces (G9-B14).
  return JSON.stringify([row.database_name || '', row.ledger_key || row.id])
}
