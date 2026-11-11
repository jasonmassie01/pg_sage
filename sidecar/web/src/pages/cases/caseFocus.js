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

// investigationCase is the case id of the focused investigation, if the
// investigations list holds it.
export function investigationCase(focus, investigations) {
  if (focus?.kind !== 'investigation') return null
  const inv = (investigations || []).find(i => String(i.id) === focus.id &&
    sameDatabase(focus, i.database))
  return inv?.case_id || null
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
