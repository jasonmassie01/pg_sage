// advisors-08b: which database of the fleet owns this database's instance
// settings (WAL, connections, ALTER SYSTEM), from the fleet status field
// status.capabilities.instance_owner (advisors-08). Pure functions.

// OWNER_NAME_MAX is the longest owner name shown in full, in characters
// (code points); a longer one is cut to this many characters, ellipsis
// included, with the full name in the title.
export const OWNER_NAME_MAX = 64

const UNKNOWN = 'Instance owner unknown'

function ownership(caps) {
  const value = caps?.instance_owner
  if (value === undefined || value === null) return null
  if (typeof value !== 'object' || Array.isArray(value)) return {}
  return Object.keys(value).length === 0 ? null : value
}

function ownerName(o) {
  return typeof o.owner === 'string' && o.owner.trim() !== '' ? o.owner : null
}

function reasonOf(o) {
  return typeof o.reason === 'string' && o.reason !== '' ? o.reason : undefined
}

function lineFor(o, name) {
  if (o.is_owner === true) return 'Owns the instance settings'
  // Only an explicit false defers; any other is_owner is unknown.
  if (o.is_owner === false && name) return `Instance settings handled by ${name}`
  return UNKNOWN
}

// instanceOwnerText is the full line for caps (a status.capabilities
// object), or null when it reports no instance owner.
export function instanceOwnerText(caps) {
  const o = ownership(caps)
  if (!o) return null
  return lineFor(o, ownerName(o))
}

// instanceOwnerLabel is the line to render, { text, title }, or null. A
// long owner name is truncated in text and given in full as the title;
// otherwise the title is the backend's reason, when it gave one.
export function instanceOwnerLabel(caps) {
  const o = ownership(caps)
  if (!o) return null
  const name = ownerName(o)
  const deferred = o.is_owner === false && name !== null
  // Count and cut code points, so a cut never splits a surrogate pair.
  const chars = deferred ? Array.from(name) : []
  if (chars.length > OWNER_NAME_MAX) {
    const shown = `${chars.slice(0, OWNER_NAME_MAX - 1).join('')}\u2026`
    return { text: lineFor(o, shown), title: name }
  }
  return { text: lineFor(o, name), title: reasonOf(o) }
}
