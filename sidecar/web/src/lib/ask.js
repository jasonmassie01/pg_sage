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
  ['trust', '#/trust'], ['facts', '#/facts'],
])

export function citationHref(kind) {
  return CITATION_ROUTES.get(kind) ?? null
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

// citationNumbers maps each citation id to its 1-based position; a
// duplicated id keeps its first number.
export function citationNumbers(citations) {
  const out = Object.create(null)
  let n = 0
  for (const c of citations || []) {
    n += 1
    if (!(c.id in out)) out[c.id] = n
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
  return { kind: 'http', message: fallback }
}
