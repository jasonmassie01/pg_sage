import { formatTime } from '../../lib/time'
// Runtime state of one fleet database, as GET /api/v1/databases reports
// it, in the words the Fleet and Database pages show (fleet-06).

export const DASH = '—'

// RUNTIME_HEADERS are the column titles of RuntimeCells, in order.
export const RUNTIME_HEADERS = [
  'Connection', 'Findings', 'Health', 'Effective trust', 'Emergency stop',
  'LLM budget', 'Explore',
]

// joinRuntime pairs managed registrations with runtime rows: on
// database_id when both sides carry one above 0, otherwise on name. A
// same-named runtime row of another database id is never joined.
export function joinRuntime(managed, runtime) {
  const byID = new Map()
  const byName = new Map()
  for (const row of runtime) {
    if (row.database_id > 0) byID.set(row.database_id, row)
    byName.set(row.name, row)
  }
  const joined = new Map()
  for (const db of managed) {
    let row = db.id > 0 ? byID.get(db.id) : undefined
    if (!row) {
      const named = byName.get(db.name)
      if (named && !(db.id > 0 && named.database_id > 0)) row = named
    }
    if (row) joined.set(db, row)
  }
  const used = new Set(joined.values())
  return {
    runtimeFor: db => joined.get(db),
    unjoined: runtime.filter(row => !used.has(row)),
  }
}

// connectionLabel is "connected", "disconnected" or "unknown" for a
// runtime row; without one it is "not running" when the runtime inventory
// was read, and "unknown" when it could not be.
export function connectionLabel(runtime, runtimeKnown = true) {
  if (!runtime) return runtimeKnown ? 'not running' : 'unknown'
  const connected = runtime.status?.connected
  if (connected === true) return 'connected'
  if (connected === false) return 'disconnected'
  return 'unknown'
}

// haLabel is the HA role and safe mode when the HA monitor knows them.
export function haLabel(status) {
  const parts = []
  if (status?.ha_role && status.ha_role !== 'unknown') parts.push(status.ha_role)
  if (status?.ha_safe_mode) parts.push('HA safe mode')
  return parts.join(' · ')
}

export function findingsLabel(status) {
  if (!Number.isFinite(status?.findings_open)) return DASH
  const critical = Number.isFinite(status.findings_critical) ? status.findings_critical : 0
  return `${status.findings_open} open · ${critical} critical`
}

export function healthLabel(status) {
  return Number.isFinite(status?.health_score) ? String(status.health_score) : DASH
}

export function healthReasons(status) {
  return Array.isArray(status?.health_reasons)
    ? status.health_reasons.filter(r => typeof r === 'string' && r) : []
}

// trustState is the effective trust and whether it overrides the
// registered level; with no registration there is nothing to override.
export function trustState(status, registered) {
  const effective = status?.trust_level || 'unknown'
  const override = Boolean(registered && status?.trust_level &&
    status.trust_level !== registered)
  return { effective, override }
}

// formatRun is a local time, or "never" for a missing or Go zero time.
export function formatRun(value) {
  if (!value) return 'never'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() < 1971) return 'never'
  return formatTime(date)
}

export function stopLabel(runtime) {
  if (!runtime) return DASH
  if (!runtime.emergency_stopped) return 'not stopped'
  const by = runtime.emergency_stopped_by || 'an unknown actor'
  const at = runtime.emergency_stopped_at ? formatRun(runtime.emergency_stopped_at) : 'never'
  return at === 'never' ? `stopped by ${by}` : `stopped by ${by} at ${at}`
}

// budgetState is "used / allocation" and whether the share is spent, or a
// dash when no fleet budget covers the database.
export function budgetState(runtime) {
  const budget = runtime?.llm_budget
  if (!budget || !Number.isFinite(budget.allocation) || !Number.isFinite(budget.used)) {
    return { label: DASH, exhausted: false }
  }
  return {
    label: `${budget.used} / ${budget.allocation}`,
    exhausted: budget.used >= budget.allocation,
  }
}
