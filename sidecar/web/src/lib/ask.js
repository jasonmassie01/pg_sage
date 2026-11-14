// ask.js — pure helpers for the Ask Sage page: API URL builders, the
// evidence-kind -> UI route mapping, status labels, digest shortening,
// citation numbering and error classification.

export const MAX_QUESTION_LENGTH = 2000

const enc = encodeURIComponent

export function askURL(db) {
  return `/api/v1/databases/${enc(db)}/ask`
}

export function conversationsURL(db) {
  return `${askURL(db)}/conversations`
}

export function conversationURL(db, id) {
  return `${conversationsURL(db)}/${enc(id)}`
}

// Asynchronous runs (mcp-07b): start, poll, cancel.
export function runsURL(db) {
  return `${askURL(db)}/runs`
}

export function runURL(db, id) {
  return `${runsURL(db)}/${enc(id)}`
}

export function cancelRunURL(db, id) {
  return `${runURL(db, id)}/cancel`
}

export function budgetURL(db) {
  return `${askURL(db)}/budget`
}

// isSingleDatabase is true when one named database is selected (not the
// fleet-wide "all" and not nothing).
export function isSingleDatabase(db) {
  if (typeof db !== 'string') return false
  const name = db.trim()
  return name !== '' && name !== 'all'
}

// Evidence kinds that have a page in this UI. Kinds not listed (config,
// doc, table, queries, anything unknown) render as plain text. A Map keeps
// prototype keys such as "__proto__" from ever resolving to a route.
const CITATION_ROUTES = new Map([
  ['finding', '#/cases'], ['findings', '#/cases'],
  ['investigation', '#/cases'], ['investigations', '#/cases'],
  ['incidents', '#/cases'],
  ['action', '#/actions'], ['actions', '#/actions'],
  ['approvals', '#/actions'], ['proposal', '#/actions'],
  ['trust', '#/trust'], ['facts', '#/facts'], ['fact', '#/facts'],
])

// Singular kinds deep-link to the object: the page focuses it from this
// query parameter (Cases resolves a finding or an investigation to its
// case; Actions focuses the change record).
const CITATION_FOCUS = new Map([
  ['finding', 'finding'], ['investigation', 'investigation'], ['action', 'action'],
])

// citationRef is a citation's reference: its ref, else the part of its id
// after the kind (answers stored without a ref).
function citationRef(c) {
  if (typeof c.ref === 'string' && c.ref) return c.ref
  const id = typeof c.id === 'string' ? c.id : ''
  const at = id.indexOf(':')
  return at >= 0 ? id.slice(at + 1) : ''
}

// citationHref is where a citation links in this UI, or null when its
// kind has no page. The reference only ever lands in a URL-encoded query
// value, so it can never change the route.
export function citationHref(c, database) {
  if (!c || typeof c !== 'object' || typeof c.kind !== 'string') return null
  const root = CITATION_ROUTES.get(c.kind)
  if (!root) return null
  const param = CITATION_FOCUS.get(c.kind)
  const ref = param ? citationRef(c) : ''
  if (!ref) return root
  const query = new URLSearchParams({ [param]: ref })
  if (isSingleDatabase(database)) query.set('database', database.trim())
  return `${root}?${query}`
}

// citationKey identifies one cited read: its alias (two reads of one id
// are two citations), else its id (answers stored before aliases).
export function citationKey(c) {
  return c?.alias || c?.id
}

// statementRefs is what a statement cites, as citation keys.
export function statementRefs(s) {
  if (Array.isArray(s?.aliases) && s.aliases.length) return s.aliases
  return Array.isArray(s?.citations) ? s.citations : []
}

const STATUS_LABELS = new Map([
  ['answered', 'Answered'], ['not_observed', 'Not observed'],
  ['budget_exhausted', 'Budget exhausted'], ['llm_unavailable', 'LLM unavailable'],
  ['incomplete', 'Incomplete'],
])

export function statusLabel(status) {
  if (!status) return 'Unknown'
  return STATUS_LABELS.get(status) ?? String(status)
}

export function shortDigest(digest) {
  return digest ? String(digest).slice(0, 12) : ''
}

// citationNumbers maps each citation key (alias, else id) to its 1-based
// position; a duplicated key keeps its first number.
export function citationNumbers(citations) {
  const out = Object.create(null)
  let n = 0
  for (const c of citations || []) {
    n += 1
    const key = citationKey(c)
    if (!(key in out)) out[key] = n
  }
  return out
}

// describeAskError turns an HTTP error status and its {error, code} body
// into a kind the page can branch on and a message to show.
export function describeAskError(status, body) {
  const server = typeof body?.error === 'string' && body.error ? body.error : ''
  const fallback = server || `Request failed (HTTP ${status})`
  if (status === 401) return { kind: 'auth', message: 'Your session expired; sign in again.' }
  if (status === 503 && body?.code === 'ask_disabled') {
    return { kind: 'disabled', message: 'Ask Sage is disabled' }
  }
  if (status === 503) return { kind: 'unavailable', message: fallback }
  if (status === 400) return { kind: 'invalid', message: fallback }
  if (status === 404) return { kind: 'not_found', message: fallback }
  if (status === 409) return { kind: 'conflict', message: fallback }
  if (status === 429) return { kind: 'busy', message: fallback }
  return { kind: 'http', message: fallback }
}
