// databaseFilter: the fleet text filter (ux-05e, win 19). A query matches
// a database's name, host, or a tag (its key, its value or key=value),
// case-insensitively and as plain text: no character is a pattern, so a
// typed ".*" finds a name that contains ".*" and nothing else.

function tagTerms(tags) {
  if (!tags || typeof tags !== 'object' || Array.isArray(tags)) return []
  return Object.entries(tags).flatMap(([k, v]) => [k, String(v), `${k}=${v}`])
}

function normalized(query) {
  return String(query ?? '').trim().toLowerCase()
}

function matchesNormalized(db, q) {
  if (!q) return true
  if (!db) return false
  const terms = [db.name, db.host, ...tagTerms(db.tags)]
  return terms.some(t => typeof t === 'string' && t.toLowerCase().includes(q))
}

// matchesDatabase is true when db matches query; a blank query matches all.
export function matchesDatabase(db, query) {
  return matchesNormalized(db, normalized(query))
}

// filterDatabases keeps the databases matching query, in order: the list
// itself for a blank query, [] for no list.
export function filterDatabases(databases, query) {
  if (!Array.isArray(databases)) return []
  const q = normalized(query)
  if (!q) return databases
  return databases.filter(db => matchesNormalized(db, q))
}
