import { caseID } from './casePresentation'
import { findRoute } from '../../routes'

// caseFocus: what the Cases page focuses from its hash. A case link names
// the case (case=<id>); an Ask Sage citation names the finding
// (finding=<id>) or the investigation (investigation=<id>) it read, and
// the page resolves that to the case it belongs to. Case ids are identity
// keys, never finding ids, so a finding resolves only through the
// source_ids of a finding-backed case of the same database.

const FOCUS_KINDS = ['case', 'finding', 'investigation']
const FINDING_SOURCES = new Set(['finding', 'forecast', 'schema_health'])

// focusFromHash is {kind, id, database} for a focused Issues hash (#/issues
// or one of its old paths, ux-11), else null.
export function focusFromHash(hash = window.location.hash) {
  const [, query = ''] = String(hash || '').split('?')
  if (!hash || findRoute(hash)?.id !== 'cases') return null
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

// INVESTIGATION_ID is a canonical investigation id (uuidPattern in
// internal/sre/ids.go). The detail route does not canonicalise its path
// value, so an id is lower-cased here before it is asked for.
const INVESTIGATION_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

// investigationID is id as a canonical lower-case UUID, or '' when it is
// not one (and so names no investigation).
export function investigationID(id) {
  if (typeof id !== 'string') return ''
  const low = id.toLowerCase()
  return INVESTIGATION_ID.test(low) ? low : ''
}

// focusDatabase is the database a focus belongs to: the one it names,
// otherwise the selected one; '' in the fleet view with none named.
export function focusDatabase(focus, database) {
  if (focus?.database) return focus.database
  return database && database !== 'all' ? database : ''
}

// investigationDetailURL is the per-id read of the focused investigation
// (cases-12), or null when the focus names no valid id or no database.
// A cited investigation resolves by id, never through the database's
// newest 100, so an older one still opens its case.
export function investigationDetailURL(focus, database) {
  if (focus?.kind !== 'investigation') return null
  const id = investigationID(focus.id)
  const db = focusDatabase(focus, database)
  if (!id || !db) return null
  return `/api/v1/databases/${encodeURIComponent(db)}/investigations/${encodeURIComponent(id)}`
}

// focusedInvestigation is the investigation read for the focus, if it is
// the focused one of the focused database.
export function focusedInvestigation(focus, investigation) {
  if (focus?.kind !== 'investigation' || !investigation) return null
  const id = investigationID(focus.id)
  if (!id || String(investigation.id) !== id) return null
  return sameDatabase(focus, investigation.database) ? investigation : null
}

// investigationCase is the case id of the focused investigation, if it
// belongs to a case.
export function investigationCase(focus, investigation) {
  const caseID = focusedInvestigation(focus, investigation)?.case_id
  return isCaseID(caseID) ? caseID : null
}

// resolveFocus is the case id to focus, or null when nothing matches.
export function resolveFocus(focus, cases, investigation) {
  if (!focus) return null
  if (focus.kind === 'case') return focus.id
  if (focus.kind === 'investigation') return investigationCase(focus, investigation)
  const found = (cases || []).find(c => FINDING_SOURCES.has(c.source_type) &&
    sameDatabase(focus, c.database_name) &&
    (c.source_ids || []).some(id => String(id) === focus.id))
  return found ? caseID(found) : null
}
