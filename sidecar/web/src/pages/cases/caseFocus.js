import { caseID } from './casePresentation'

// caseFocus: what the Cases page focuses from its hash. A case link names
// the case (case=<id>); an Ask Sage citation names the finding
// (finding=<id>) or the investigation (investigation=<id>) it read, and
// the page resolves that to the case it belongs to. Case ids are identity
// keys, never finding ids, so a finding resolves only through the
// source_ids of a finding-backed case of the same database.

const FOCUS_KINDS = ['case', 'finding', 'investigation']
const FINDING_SOURCES = new Set(['finding', 'forecast', 'schema_health'])

// focusFromHash is {kind, id, database} for a focused Cases hash, else null.
export function focusFromHash(hash = window.location.hash) {
  const [path, query = ''] = String(hash || '').split('?')
  if (path !== '#/cases') return null
  const params = new URLSearchParams(query)
  for (const kind of FOCUS_KINDS) {
    const id = params.get(kind)
    if (id) return { kind, id, database: params.get('database') || '' }
  }
  return null
}

const sameDatabase = (focus, name) => !focus.database || !name || name === focus.database

// Case id kinds the cases API accepts as a focus (caseIdentityKinds in
// internal/api/cases_focus.go). An investigation Ask Sage opened has a
// synthetic ask:<digest> case id that names no case.
const CASE_ID_KINDS = new Set(['finding', 'incident', 'query_hint'])

function isCaseID(id) {
  if (typeof id !== 'string') return false
  const at = id.indexOf(':')
  return at > 0 && at < id.length - 1 && CASE_ID_KINDS.has(id.slice(0, at))
}

// focusedInvestigation is the focused investigation, if the
// investigations list holds it for the focused database.
export function focusedInvestigation(focus, investigations) {
  if (focus?.kind !== 'investigation') return null
  return (investigations || []).find(i => String(i.id) === focus.id &&
    sameDatabase(focus, i.database)) || null
}

// investigationCase is the case id of the focused investigation, if the
// investigations list holds it and it belongs to a case.
export function investigationCase(focus, investigations) {
  const caseID = focusedInvestigation(focus, investigations)?.case_id
  return isCaseID(caseID) ? caseID : null
}

// resolveFocus is the case id to focus, or null when nothing matches.
export function resolveFocus(focus, cases, investigations) {
  if (!focus) return null
  if (focus.kind === 'case') return focus.id
  if (focus.kind === 'investigation') return investigationCase(focus, investigations)
  const found = (cases || []).find(c => FINDING_SOURCES.has(c.source_type) &&
    sameDatabase(focus, c.database_name) &&
    (c.source_ids || []).some(id => String(id) === focus.id))
  return found ? caseID(found) : null
}
