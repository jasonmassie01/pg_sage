// Keep database identity with every relation: finding and queue IDs are local
// to a database. A missing relation remains unknown instead of matching text.
export function caseID(row) {
  return row?.case_id || row?.id || row?.identity_key || ''
}

export function rowDatabase(row, fallback) {
  return row?.database_name || row?.database || (fallback !== 'all' ? fallback : '') || ''
}

export function relatedCase(row, cases, database) {
  const db = rowDatabase(row, database)
  return (cases || []).find(c => {
    if (!db || c.database_name !== db) return false
    if (row.case_id) return String(caseID(c)) === String(row.case_id)
    const finding = row.finding_id ?? row.finding?.id
    return finding != null && ['finding', 'schema_health', 'forecast'].includes(c.source_type)
      && (c.source_ids || []).some(id => String(id) === String(finding))
  })
}

export function caseHref(row, database) {
  const params = new URLSearchParams()
  if (caseID(row)) params.set('case', caseID(row))
  const db = rowDatabase(row, database)
  if (db) params.set('database', db)
  return `#/cases${params.size ? `?${params}` : ''}`
}

export function riskLabel(value) {
  return ({ safe: 'Low', moderate: 'Moderate', high: 'High', high_risk: 'High', unknown: 'Unknown' })[value]
    || (value ? value.replaceAll('_', ' ') : 'Unknown')
}

export function actionLabel(value) {
  const labels = {
    analyze: 'Update query-planning statistics', analyze_table: 'Update query-planning statistics',
    vacuum: 'Clean up expired table rows', vacuum_table: 'Clean up expired table rows',
    create_index: 'Add an index', create_index_concurrently: 'Add an index',
    drop_index: 'Remove an index', drop_unused_index: 'Remove an unused index',
    reindex: 'Rebuild an index', reindex_concurrently: 'Rebuild an index',
  }
  if (labels[value]) return labels[value]
  const text = (value || 'Change details unavailable').replaceAll('_', ' ')
  return text.charAt(0).toUpperCase() + text.slice(1)
}

export function blockerText(value) {
  const labels = {
    observation_mode: 'Database changes are disabled in Observation mode.',
    observation: 'Database changes are disabled in Observation mode.',
    provider_unsupported: 'pg_sage cannot run this action for this connection.',
    insufficient_privilege: 'This connection lacks permission for the proposed change.',
    permission_denied: 'This connection lacks permission for the proposed change.',
    learning_baseline: 'Waiting for sufficient disk-load history.',
    maintenance_window: 'Waiting for an allowed maintenance window.',
  }
  return labels[value] || (value ? value.replaceAll('_', ' ') : '')
}

export function inProgress(row) {
  const state = row.lifecycle_state || row.status || row.outcome
  if (row.record_kind === 'queued' && state === 'pending') return false
  if (row.record_kind === 'executed' && state === 'pending') return true
  if (['approved', 'queued', 'executing', 'applying', 'applied', 'verifying',
    'monitoring', 'applied_pending_restart'].includes(state)) return true
  const verdict = row.verification_outcome?.verdict || row.verification_status
  return !['failed', 'expired', 'rejected', 'rolled_back', 'reverted'].includes(state)
    && ['pending', 'monitoring', 'verifying'].includes(verdict)
}

export function awaitingReview(row) {
  return row.record_kind === 'queued' && (row.status || row.outcome) === 'pending'
}

// Presentation aliases only. Matching a family never grants execution rights or
// makes the stored revision's SQL, risk or recovery equivalent to today's plan.
export function actionFamily(actionType) {
  return ({ create_index_concurrently: 'create_index', analyze_table: 'analyze',
    vacuum_table: 'vacuum', drop_unused_index: 'drop_index',
    reindex_concurrently: 'reindex' })[actionType] || actionType
}

export function proposedChanges(recommendations, cases, database) {
  const rows = (recommendations || []).filter(r => r.state === 'proposed').map(r => {
    const linked = relatedCase(r, cases, database)
    const candidates = linked?.action_candidates || []
    const currentCandidate = candidates.find(c => c.action_type === r.action_type)
      || candidates.find(c => actionFamily(c.action_type) === actionFamily(r.action_type))
    return { ...r, linkedCase: linked, currentCandidate, storedRecommendation: true,
      database_name: rowDatabase(r, database),
      key: `recommendation:${rowDatabase(r, database)}:${r.id}` }
  })
  for (const c of cases || []) {
    for (const candidate of c.action_candidates || []) {
      const represented = (recommendations || []).some(r =>
        relatedCase(r, [c], database) && actionFamily(r.action_type) === actionFamily(candidate.action_type))
      if (represented) continue
      rows.push({ ...candidate, linkedCase: c, database_name: c.database_name,
        state: 'proposed', key: `case:${c.database_name}:${caseID(c)}:${candidate.action_type}` })
    }
  }
  return rows
}

// A persisted revision must not reintroduce a procedure that the current
// provider-aware projection has withheld, including older action-type names.
export function storedProcedureRestriction(row) {
  const current = row.currentCandidate || row.linkedCase?.action_candidates?.find(c =>
    c.policy_decision?.blocked_reason === 'provider_managed_setting'
      || c.blocked_reason === 'provider_managed_setting')
  if (!current) return ''
  const providerManaged = current.policy_decision?.blocked_reason === 'provider_managed_setting'
    || current.blocked_reason === 'provider_managed_setting'
  if (providerManaged || current.rollback_class === 'not_applicable') {
    return 'Stored SQL is withheld because the current case guidance does not identify an applicable database-change procedure. '
      + (current.blocked_reason && current.blocked_reason !== 'provider_managed_setting'
        ? current.blocked_reason : current.recovery_detail
          || (providerManaged ? 'The connection provider manages this setting; no replacement procedure has been validated.'
            : 'The current guidance does not propose an applicable database change.'))
  }
  return ''
}

// Stored procedures can predate a corrected case projection. Show runnable
// recovery only when it matches the complete current reversible procedure;
// action-family aliases and a shared finding are not validation of old SQL.
export function recoveryForRevision(row, revision) {
  const stored = revision?.inverse_sql?.trim()
  if (!stored) return { sql: '', reason: '' }
  const restriction = storedProcedureRestriction(row)
  if (restriction) return { sql: '', reason: restriction }
  const current = row.currentCandidate
  if (!current || current.rollback_class !== 'reversible') {
    return { sql: '', reason: 'Stored recovery SQL is withheld because a validated reversible recovery plan has not been established for the current case proposal.' }
  }
  const currentForward = current.proposed_sql?.trim()
  const currentInverse = current.script_output?.rollback_sql?.trim()
  if (!currentForward || !currentInverse
    || revision.forward_sql?.trim() !== currentForward || stored !== currentInverse) {
    return { sql: '', reason: 'Stored recovery SQL is withheld because it has not been validated against the current case proposal and recovery procedure.' }
  }
  return { sql: stored, reason: '' }
}
