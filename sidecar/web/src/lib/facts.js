// Binding facts (roadmap 2.3): API helpers and the declare-form rules
// shared by the Facts page and the inline fact badges. Confirmed facts
// only narrow or redirect what pg_sage does on its own; proposed facts
// change nothing until a person confirms them.

export const FACT_TYPES = [
  'owned_by_app_migrations', 'test_fixture', 'slot_consumer', 'append_only',
  'table_window',
]

// KINDS_BY_TYPE is the subject kinds each fact type may be about.
export const KINDS_BY_TYPE = {
  owned_by_app_migrations: ['index', 'table', 'schema'],
  test_fixture: ['schema'],
  slot_consumer: ['slot'],
  append_only: ['table'],
  table_window: ['table'],
}

export const SUBJECT_PLACEHOLDERS = {
  index: 'public.idx_orders_status',
  table: 'public.orders',
  schema: 'test_*',
  slot: 'debezium_orders',
}

export const WINDOW_KINDS = ['maintenance', 'batch']

export function canDecideFacts(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

function dbQuery(database) {
  return `?database=${encodeURIComponent(database || 'all')}`
}

// factsListURL lists a database's facts ("all" for the fleet); a falsy
// status lists every status.
export function factsListURL(database, status) {
  const base = `/api/v1/facts${dbQuery(database)}`
  return status ? `${base}&status=${encodeURIComponent(status)}` : base
}

export function factsMatchURL(database, objects) {
  const params = objects.map(o => `&object=${encodeURIComponent(o)}`).join('')
  return `/api/v1/facts/match${dbQuery(database)}${params}`
}

// factErrorMessage turns a refused fact request into words a person can
// act on.
export function factErrorMessage(status, json) {
  if (json?.code === 'content_changed') {
    return 'This fact changed since it was shown. Reload and review it again.'
  }
  return json?.error || `Request failed (${status})`
}

async function postJSON(url, body) {
  const res = await fetch(url, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  let json = {}
  try {
    json = await res.json()
  } catch (err) {
    json = { error: `unreadable response (${res.status}: ${err.message})` }
  }
  if (!res.ok) throw new Error(factErrorMessage(res.status, json))
  return json
}

// decideFact confirms, rejects or expires one fact on its database.
export function decideFact(database, id, verb, note) {
  return postJSON(`/api/v1/facts/${id}/${verb}${dbQuery(database)}`,
    { note: note || '' })
}

export function declareFact(database, body) {
  return postJSON(`/api/v1/facts${dbQuery(database)}`, body)
}

// declareBody builds the POST /facts body from the form's fields. Only
// the value fields the fact type uses are sent.
export function declareBody(form) {
  const value = {}
  if (form.type === 'slot_consumer') value.consumer = form.consumer.trim()
  if (form.type === 'table_window') {
    value.kind = form.windowKind
    value.window = form.window.trim()
  }
  const body = { type: form.type, subject_kind: form.kind,
    subject: form.subject.trim(), value, note: form.note.trim() }
  if (form.expires) body.expires_at = new Date(form.expires).toISOString()
  return body
}

// declareReady says whether the form has what the fact type needs.
export function declareReady(form) {
  if (!form.subject.trim()) return false
  if (form.type === 'slot_consumer' && !form.consumer.trim()) return false
  if (form.type === 'table_window' && !form.window.trim()) return false
  return true
}

// formatFactDate shows a timestamp, or null for unset ones (null, or a
// Go zero time).
export function formatFactDate(value) {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() < 1971) return null
  return date.toLocaleString()
}

// collectMatches flattens a /facts/match response into the distinct
// confirmed and proposed facts about the asked objects.
export function collectMatches(data) {
  const confirmed = new Map()
  const proposed = new Map()
  const objects = data?.objects
  if (!objects || typeof objects !== 'object') return { confirmed: [], proposed: [] }
  for (const entry of Object.values(objects)) {
    for (const f of asList(entry?.confirmed)) confirmed.set(f.id, f)
    for (const f of asList(entry?.proposed)) proposed.set(f.id, f)
  }
  return { confirmed: [...confirmed.values()], proposed: [...proposed.values()] }
}

function asList(value) {
  return Array.isArray(value) ? value.filter(f => f && f.id != null) : []
}
