// fleetList: the server-side fleet table query (ux-10a). GET
// /api/v1/databases with q, tag, sort, order and limit searches, filters,
// sorts and pages the fleet in the sidecar; this builds that URL and joins
// its rows to the managed registrations.
import { joinRuntime } from './runtimeStatus'

export const FLEET_PAGE_SIZE = 50

// FLEET_SORTS maps a sortable column (Database is the read-only table's
// name column) to its server sort and the order the
// server uses when none is given (problems first: lowest health, most
// open findings).
export const FLEET_SORTS = {
  Name: { sort: 'name', order: 'asc' },
  Database: { sort: 'name', order: 'asc' },
  'Execution Mode': { sort: 'mode', order: 'asc' },
  Findings: { sort: 'open_issues', order: 'desc' },
  Health: { sort: 'health', order: 'asc' },
}

export const DEFAULT_SORT = { sort: 'name', order: 'asc' }

// fleetListURL is the first-page URL for a view: the trimmed search, each
// tag filter once, the sort and its order. It always names sort and limit,
// so the server answers with a page, never the whole fleet.
export function fleetListURL({ q = '', tags = [], sort = DEFAULT_SORT,
  limit = FLEET_PAGE_SIZE } = {}) {
  const params = new URLSearchParams()
  const search = String(q).trim()
  if (search) params.set('q', search)
  for (const tag of new Set(tags.map(t => String(t).trim()).filter(Boolean))) {
    params.append('tag', tag)
  }
  params.set('sort', sort.sort)
  params.set('order', sort.order)
  params.set('limit', String(limit))
  return `/api/v1/databases?${params}`
}

// nextSort is the sort after a click on column: a new column starts at
// its default order, the current one flips.
export function nextSort(current, column) {
  const target = FLEET_SORTS[column]
  if (!target) return current
  if (current.sort !== target.sort) return { ...target }
  return { sort: target.sort, order: current.order === 'asc' ? 'desc' : 'asc' }
}

// ariaSort is a column header's aria-sort for the current sort.
export function ariaSort(current, column) {
  const target = FLEET_SORTS[column]
  if (!target) return undefined
  if (current.sort !== target.sort) return 'none'
  return current.order === 'asc' ? 'ascending' : 'descending'
}

// fleetRows pairs each runtime row of a page with its registration (by
// database id, else by name, as joinRuntime does). A row with none comes
// from the config file and has registration null.
export function fleetRows(registrations, runtimeRows) {
  const { runtimeFor } = joinRuntime(registrations, runtimeRows)
  const byRow = new Map()
  for (const reg of registrations) {
    const row = runtimeFor(reg)
    if (row) byRow.set(row, reg)
  }
  return runtimeRows.map(runtime => ({
    key: runtime.name, runtime, registration: byRow.get(runtime) || null,
  }))
}

// addTag is tags with one more filter, trimmed; a blank or repeated tag,
// or one with no key ("=prod", which the server refuses), changes nothing.
export function addTag(tags, raw) {
  const tag = String(raw ?? '').trim()
  if (!tag.split('=')[0].trim()) return tags
  if (tags.some(t => t.toLowerCase() === tag.toLowerCase())) return tags
  return [...tags, tag]
}

// parseTagAssignment reads a bulk tag as {key, value}: "key=value", both
// trimmed and non-empty, else null (registrations hold key=value tags).
export function parseTagAssignment(raw) {
  const [key, ...rest] = String(raw ?? '').split('=')
  const value = rest.join('=').trim()
  return key.trim() && value ? { key: key.trim(), value } : null
}

// tagUpdateBody is the PUT /api/v1/databases/managed/{id} body that sets
// one tag on rec, a fresh GET of the registration. The route replaces every
// field, so each goes back as stored; an empty password and no action
// identity keep the stored ones, as the edit form does.
export function tagUpdateBody(rec, { key, value }) {
  return {
    name: rec.name, host: rec.host, port: rec.port, database_name: rec.database_name,
    username: rec.username, password: '', sslmode: rec.sslmode,
    max_connections: rec.max_connections, trust_level: rec.trust_level,
    execution_mode: rec.execution_mode, tags: { ...(rec.tags || {}), [key]: value },
  }
}
