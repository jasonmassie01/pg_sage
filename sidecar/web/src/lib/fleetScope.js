// fleetScope: the header scope at fleet scale (ux-10b). The scope is "all",
// a tag scope (?tag=k=v, read by the pages that support it) or one
// database. From FLEET_SUMMARY_FROM databases up the "all" scope renders
// fleet summaries instead of one block per database, and the picker
// searches the fleet list on the server, PICKER_PAGE names a page.
import { fleetListURL } from '../pages/databases/fleetList'
import { readPref, writePref } from './storage'

// FLEET_SUMMARY_FROM is the fleet size from which "all" renders summaries
// and the picker becomes a searched combobox; a smaller fleet keeps its
// per-database detail and the plain select.
export const FLEET_SUMMARY_FROM = 6
export const PICKER_PAGE = 50
export const RECENT_MAX = 5
// SUMMARY_LIST is how many databases a fleet summary lists at a time.
export const SUMMARY_LIST = 10

const RECENT_KEY = 'pg_sage_recent_dbs'
const NAME_SORT = { sort: 'name', order: 'asc' }

// summarizes is true when the scope is the whole fleet and the answer
// covers enough databases to be read as a summary.
export function summarizes(database, count) {
  const all = !database || database === 'all'
  return all && Number.isFinite(count) && count >= FLEET_SUMMARY_FROM
}

// parseTagScope reads "tag:key=value" (or "tag:bare") typed into the
// picker; null when the text is not a tag scope or names no key.
export function parseTagScope(text) {
  const match = /^\s*tag:(.*)$/i.exec(String(text ?? ''))
  if (!match) return null
  const tag = match[1].trim()
  if (!tag || tag.startsWith('=')) return null
  return tag
}

// databaseSearchURL is one picker page of the fleet list (ux-10a), by name.
export function databaseSearchURL({ q = '', tag = '', cursor = '' } = {}) {
  const url = fleetListURL({ q, tags: tag ? [tag] : [], sort: NAME_SORT,
    limit: PICKER_PAGE })
  return cursor ? `${url}&cursor=${encodeURIComponent(cursor)}` : url
}

// readRecent is the recently selected database names, newest first.
export function readRecent() {
  let parsed
  try {
    parsed = JSON.parse(readPref(RECENT_KEY, '[]'))
  } catch (err) {
    if (err instanceof SyntaxError) return []
    throw err
  }
  if (!Array.isArray(parsed)) return []
  return parsed.filter(n => typeof n === 'string' && n !== '').slice(0, RECENT_MAX)
}

// pushRecent records name as the most recent database and returns the list.
export function pushRecent(name) {
  if (typeof name !== 'string' || name === '' || name === 'all') return readRecent()
  const next = [name, ...readRecent().filter(n => n !== name)].slice(0, RECENT_MAX)
  writePref(RECENT_KEY, JSON.stringify(next))
  return next
}
