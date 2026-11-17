import { useSyncExternalStore } from 'react'
import { readPref, writePref } from './storage'

// time.js: the one way the dashboard prints a point in time (ux-05d). Every
// time names its zone, "2026-11-15 14:05 CST" in the viewer's local zone or
// "2026-11-15 20:05 UTC", so a person approving "for the next window"
// compares one clock. The viewer picks local or UTC in the header; the
// choice is remembered in this browser.

export const TIME_ZONE_PREF = 'pg_sage_time_zone'
const MODES = ['local', 'utc']
const MISSING = '—'
const UNKNOWN = 'unknown time'

let mode = null
const listeners = new Set()
const formatters = new Map()

// timeZoneMode is the viewer's choice, "local" or "utc"; an unknown or
// missing stored value is local.
export function timeZoneMode() {
  if (mode === null) mode = readPref(TIME_ZONE_PREF, 'local') === 'utc' ? 'utc' : 'local'
  return mode
}

// setTimeZoneMode switches every formatTime call that names no zone. With
// storage blocked the choice still holds for this page load.
export function setTimeZoneMode(next) {
  if (!MODES.includes(next)) {
    throw new Error(`unknown time zone mode "${next}": use local or utc`)
  }
  if (next === timeZoneMode()) return
  mode = next
  writePref(TIME_ZONE_PREF, next)
  listeners.forEach(listener => listener())
}

// subscribeTimeZoneMode calls listener after each change and returns the
// function that stops it.
export function subscribeTimeZoneMode(listener) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

// useTimeZoneMode re-renders its component when the viewer switches zones.
// App calls it, so every time on the page is formatted again.
export function useTimeZoneMode() {
  return useSyncExternalStore(subscribeTimeZoneMode, timeZoneMode, timeZoneMode)
}

function formatterFor(zone) {
  const timeZone = zone === 'local' ? undefined : zone === 'utc' ? 'UTC' : zone
  const key = timeZone || 'local'
  if (!formatters.has(key)) {
    try {
      formatters.set(key, new Intl.DateTimeFormat('en-US', {
        timeZone, year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZoneName: 'short',
      }))
    } catch (err) {
      if (err instanceof RangeError) {
        throw new RangeError(`formatTime: unknown time zone "${zone}"`)
      }
      throw err
    }
  }
  return formatters.get(key)
}

function toDate(value) {
  if (value instanceof Date) return value
  if (typeof value === 'string' || typeof value === 'number') return new Date(value)
  return null
}

// zonedParts splits value into its year to minute fields and zone name in
// zone (the viewer's choice when none is given), or returns the text for a
// missing ("—") or unreadable ("unknown time") value.
function zonedParts(value, zone) {
  if (value === null || value === undefined || value === '') return MISSING
  const date = toDate(value)
  if (!date || Number.isNaN(date.getTime())) return UNKNOWN
  const parts = {}
  for (const part of formatterFor(zone || timeZoneMode()).formatToParts(date)) {
    parts[part.type] = part.value
  }
  return parts
}

// formatTime prints value (a Date, an ISO string or epoch milliseconds) as
// "YYYY-MM-DD HH:MM ZONE". zone is "local", "utc" or an IANA zone name and
// defaults to the viewer's choice. A missing value prints an em dash and an
// unreadable one "unknown time", never "Invalid Date".
export function formatTime(value, { zone } = {}) {
  const p = zonedParts(value, zone)
  if (typeof p === 'string') return p
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute} ${p.timeZoneName}`
}

// formatClock prints only the time of day with its zone, "14:05 CST", for
// times that are plainly today: a last collection, a budget reset, a chart
// tick. It follows the viewer's choice and treats bad input as formatTime.
export function formatClock(value, { zone } = {}) {
  const p = zonedParts(value, zone)
  if (typeof p === 'string') return p
  return `${p.hour}:${p.minute} ${p.timeZoneName}`
}

// intlTimeZone is the timeZone option for a caller that keeps its own
// Intl format (with timeZoneName), so it still follows the viewer's choice.
export function intlTimeZone() {
  return timeZoneMode() === 'utc' ? 'UTC' : undefined
}
