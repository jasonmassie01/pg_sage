// Plain-text views of a policy set's selectors, requirements and members
// (fleetpol-01c), the wire shapes of GET /api/v1/policy-sets and
// /api/v1/policy-sets/{name}/members (docs/policy-sets.md), and the
// "Governed by" line of a database.

import { formatTime } from '../../lib/time'

function tagsText(tags) {
  return Object.entries(tags || {}).map(([k, v]) => `${k}=${v}`).join(', ')
}

// selectorText is one selector: every field it names must match.
export function selectorText(sel) {
  if (!sel) return 'nothing'
  if (sel.all) return 'every database'
  const parts = []
  if (sel.tags && Object.keys(sel.tags).length) parts.push(`tags ${tagsText(sel.tags)}`)
  // No cloud adapter records a provider yet, so these match nothing today.
  if (sel.provider) parts.push(`provider ${sel.provider} (not recorded yet)`)
  if (sel.provider_project) {
    parts.push(`provider project ${sel.provider_project} (not recorded yet)`)
  }
  if (sel.discovery_source) parts.push(`discovered by ${sel.discovery_source}`)
  if (sel.creator) parts.push(`created by ${sel.creator}`)
  return parts.length ? parts.join(' and ') : 'nothing'
}

// selectorsText: a set matches when any of its selectors does.
export function selectorsText(selectors) {
  if (!selectors || selectors.length === 0) return 'no selectors: matches no database'
  return selectors.map(selectorText).join('; or ')
}

// requirementLines lists a set's requirements, [] when it has none.
export function requirementLines(req) {
  if (!req) return []
  const out = []
  for (const g of req.grades || []) {
    out.push(`${g.dimension} at least ${g.min_letter} within ${g.within_hours} h`)
  }
  if (req.owner_required) out.push('owner required')
  if (req.ttl_hours) out.push(`TTL ${req.ttl_hours} h`)
  if (req.enrollment_deadline_hours) {
    out.push(`enrolled within ${req.enrollment_deadline_hours} h`)
  }
  const ceilings = Object.entries(req.trust_ceiling || {})
  if (ceilings.length) {
    out.push(`trust ceiling ${ceilings.map(([c, l]) => `${c}: ${l}`).join(', ')}`)
  }
  return out
}

// membersText counts a set's members and those with an exception.
export function membersText(members) {
  const list = members || []
  const n = list.length
  const noun = n === 1 ? 'database' : 'databases'
  const exceptions = list.filter(m => m.exception).length
  if (!exceptions) return `${n} ${noun}`
  return `${n} ${noun} (${exceptions} with an exception)`
}

// jsonText prints one side of a diff; a side without the field is null.
export function jsonText(value) {
  if (value === undefined || value === null) return 'not set'
  return JSON.stringify(value)
}

const DIFF_FIELDS_SHOWN = 2

function changedFields(diff) {
  const fields = (diff || []).map(d => d.field)
  if (fields.length === 0) return ''
  if (fields.length <= DIFF_FIELDS_SHOWN) return fields.join(', ')
  return `${fields.slice(0, DIFF_FIELDS_SHOWN).join(', ')} and `
    + `${fields.length - DIFF_FIELDS_SHOWN} more`
}

// day is the viewer's calendar day of value, "YYYY-MM-DD".
export function day(value) {
  return formatTime(value).slice(0, 10)
}

function exceptionNote(exc) {
  const fields = changedFields(exc.diff)
  const what = fields ? `${fields} changed by ${exc.set_by}` : `same as the set, by ${exc.set_by}`
  const until = exc.expires_at ? `expires ${day(exc.expires_at)}` : 'no expiry'
  return `exception: ${what}, ${until}`
}

// governedByText is the line for databaseId, or '' without a policy.
export function governedByText(policy, exceptions, databaseId) {
  if (!policy) return ''
  const exc = (exceptions || []).find(e => e.database_id === databaseId)
  if (policy.set) return `Governed by: ${policy.set}`
  if (exc) return `Governed by: ${exc.set_name} (${exceptionNote(exc)})`
  return 'Governed by: its own policy document (no policy set matches it)'
}
