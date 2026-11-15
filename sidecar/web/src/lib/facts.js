// Binding facts (roadmap 2.3): API helpers and the declare-form rules
// shared by the Facts page and the inline fact badges. Confirmed facts
// only narrow or redirect what pg_sage does on its own; proposed facts
// change nothing until a person confirms them.

export const FACT_TYPES = [
  'owned_by_app_migrations', 'test_fixture', 'slot_consumer', 'append_only',
  'table_window', 'do_not_touch',
]

// FACT_TYPE_LABELS names the fact types the declare form shows in words;
// the others show their type name.
export const FACT_TYPE_LABELS = { do_not_touch: 'Do not touch' }

// KINDS_BY_TYPE is the subject kinds each fact type may be about.
export const KINDS_BY_TYPE = {
  owned_by_app_migrations: ['index', 'table', 'schema'],
  test_fixture: ['schema'],
  slot_consumer: ['slot'],
  append_only: ['table'],
  table_window: ['table'],
  do_not_touch: ['table', 'index', 'schema'],
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

// factCoverageURL asks which objects a subject of kind matches on one
// database today (facts-09).
export function factCoverageURL(database, kind, subject) {
  return `/api/v1/facts/coverage${dbQuery(database)}&kind=${encodeURIComponent(kind)}`
    + `&subject=${encodeURIComponent(subject)}`
}

// fetchFactCoverage returns { objects, truncated }; a refusal or a reply
// without an object list is an error, never "no match".
export async function fetchFactCoverage(database, kind, subject) {
  const res = await fetch(factCoverageURL(database, kind, subject),
    { credentials: 'include' })
  let json = {}
  try {
    json = await res.json()
  } catch (err) {
    json = { error: `unreadable response (${res.status}: ${err.message})` }
  }
  if (!res.ok) throw new Error(factErrorMessage(res.status, json))
  if (!Array.isArray(json.objects)) {
    throw new Error(`unreadable coverage response (${res.status})`)
  }
  return { objects: json.objects.map(String), truncated: json.truncated === true }
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

// The owner brief (evid-03a/03b): one confirmed fact per database (type
// owner_brief, subject kind database, subject "*"). A declaration replaces
// the whole value, so the form sends every key it read back.

export const BRIEF_FIELDS = [
  { key: 'owner', label: 'Owner', placeholder: 'Payments team' },
  { key: 'contact', label: 'Contact', placeholder: '#payments-oncall' },
  { key: 'growth', label: 'Growth', placeholder: '20 GB a month' },
  { key: 'retention', label: 'Retention', placeholder: 'orders kept 7 years' },
  { key: 'peak_windows', label: 'Peak windows',
    placeholder: 'weekdays 09:00-17:00 America/Chicago' },
  { key: 'note', label: 'Note', placeholder: 'Quiet in August' },
]

// BRIEF_MAX is the server's bound on each brief field (facts.maxValueText).
export const BRIEF_MAX = 300

export const PEAK_WINDOW_HINT = 'One or more windows separated by ";": weekdays ' +
  '09:00-17:00, mon,tue 08:00-12:00 UTC, weekends, or a 5-field cron with @duration. ' +
  'Without a time zone the sidecar\'s clock is used.'

export function ownerBriefURL(database) {
  return `/api/v1/facts${dbQuery(database)}&status=confirmed&type=owner_brief`
}

export function peakSuggestionURL(database) {
  return `/api/v1/databases/${encodeURIComponent(database)}/facts/peak-suggestion`
}

async function getJSON(url) {
  const res = await fetch(url, { credentials: 'include' })
  let json = {}
  try {
    json = await res.json()
  } catch (err) {
    json = { error: `unreadable response (${res.status}: ${err.message})` }
  }
  if (!res.ok) throw new Error(factErrorMessage(res.status, json))
  return json
}

// fetchOwnerBrief returns the database's confirmed brief, or null.
export async function fetchOwnerBrief(database) {
  const json = await getJSON(ownerBriefURL(database))
  if (!Array.isArray(json.facts)) throw new Error('unreadable owner brief response')
  return json.facts.find(f => f.type === 'owner_brief') || null
}

// fetchPeakSuggestion returns the suggestion; a reply that is neither a
// suggestion nor "none" is an error, never "none".
export async function fetchPeakSuggestion(database) {
  const json = await getJSON(peakSuggestionURL(database))
  if (json.suggestion === 'peak_windows' && typeof json.peak_windows === 'string') return json
  if (json.suggestion === 'none') return json
  throw new Error('unreadable peak-window suggestion')
}

// briefValue is the value to declare: the brief's other keys as read,
// then the form's fields, blanks left out.
export function briefValue(current, form) {
  const value = {}
  for (const [k, v] of Object.entries(current || {})) {
    if (typeof v === 'string' && v.trim()) value[k] = v
  }
  for (const { key } of BRIEF_FIELDS) {
    const v = (form[key] || '').trim()
    if (v) value[key] = v
    else delete value[key]
  }
  return value
}

// briefProblem says why the form cannot be saved yet, or null.
export function briefProblem(value) {
  if (Object.keys(value).length === 0) return 'Fill in at least one field.'
  for (const [k, v] of Object.entries(value)) {
    if (v.length > BRIEF_MAX) return `${k} is longer than ${BRIEF_MAX} characters.`
  }
  const windows = value.peak_windows
  if (windows && windows.split(';').some(part => !part.trim())) {
    return 'Peak windows has an empty window between ";" separators.'
  }
  return null
}

export function declareBrief(database, value) {
  return declareFact(database, { type: 'owner_brief', subject_kind: 'database',
    subject: '*', value, note: '' })
}
