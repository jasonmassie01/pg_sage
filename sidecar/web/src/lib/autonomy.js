// What pg_sage may run unattended right now. The source is each database's
// capabilities.action_families[].decision, which the server computes from
// the executor's standing gate on every status read (trust ramp, emergency
// stop, replica, disabled executor). Never infer autonomy from trust_level.

export const TRUST_LEVELS = ['observation', 'advisory', 'autonomous']

// Most autonomous first, for the mixed-fleet badge.
const BADGE_ORDER = ['autonomous', 'advisory', 'observation']

export function normalizeTrustLevel(level) {
  const norm = typeof level === 'string' ? level.toLowerCase() : ''
  return TRUST_LEVELS.includes(norm) ? norm : null
}

// autoFamilies lists the action types whose decision is "execute", or
// undefined when the database did not report its action families.
export function autoFamilies(caps) {
  if (!Array.isArray(caps?.action_families)) return undefined
  return caps.action_families
    .filter(f => f?.decision === 'execute'
      && typeof f.action_type === 'string' && f.action_type !== '')
    .map(f => f.action_type)
}

// rowAutoFamilies reads one fleet row. An emergency-stopped database runs
// nothing, whatever an older capabilities snapshot says.
export function rowAutoFamilies(row) {
  const families = autoFamilies(row?.status?.capabilities)
  if (row?.emergency_stopped === true) return []
  return families
}

export function fleetAutonomy(rows) {
  const levels = { observation: 0, advisory: 0, autonomous: 0 }
  const autoByFamily = {}
  let unknown = 0
  let reported = 0
  const list = Array.isArray(rows) ? rows : []
  for (const row of list) {
    const level = normalizeTrustLevel(row?.status?.trust_level)
    if (level) levels[level] += 1
    else unknown += 1
    const families = rowAutoFamilies(row)
    if (families === undefined) continue
    reported += 1
    for (const family of families) {
      autoByFamily[family] = [...(autoByFamily[family] || []), row.name]
    }
  }
  for (const names of Object.values(autoByFamily)) names.sort()
  return { levels, autoByFamily, unknown, reported, total: list.length }
}

const NOTHING_AUTO = 'Nothing auto-executes right now; actions queue for'
  + ' approval or are observe-only'

// liveHint is the tooltip for one database's families.
export function liveHint(families) {
  if (families.length === 0) return NOTHING_AUTO
  return `Auto-executes now: ${families.join(', ')}`
}

// fleetAutonomyHint is the tooltip for the fleet badge, or null when no
// database reported its families (the caller falls back to static copy).
export function fleetAutonomyHint(autonomy) {
  if (!autonomy?.reported) return null
  const families = Object.keys(autonomy.autoByFamily).sort()
  const base = families.length === 0
    ? NOTHING_AUTO
    : 'Auto-executes now: ' + families
      .map(f => `${f} on ${autonomy.autoByFamily[f].join(', ')}`).join('; ')
  const missing = unreportedNote(autonomy)
  return missing ? `${base}. ${missing}` : base
}

// unreportedNote says how many databases have not reported their action
// families, so a fleet-wide claim is never read as complete. It is null
// when every database reported, or when none did and nothing is claimed.
export function unreportedNote(autonomy) {
  if (!autonomy?.reported) return null
  const missing = autonomy.total - autonomy.reported
  if (missing <= 0) return null
  return `${missing} ${missing === 1 ? 'database has' : 'databases have'}`
    + ' not reported their action policy'
}

// fleetBadge picks the header badge for the fleet: one level renders as
// that level; more than one (an unknown level counts as its own) is Mixed.
export function fleetBadge(autonomy) {
  const present = BADGE_ORDER.filter(l => autonomy?.levels?.[l] > 0)
  if (present.length === 0) return null
  if (present.length === 1 && !autonomy.unknown) {
    return { kind: 'single', level: present[0] }
  }
  const parts = present.map(l => `${autonomy.levels[l]} ${l}`)
  if (autonomy.unknown) parts.push(`${autonomy.unknown} unknown`)
  return { kind: 'mixed', label: ['Mixed', ...parts].join(' · ') }
}

// One autonomy scale per class (ux-06b, decision D6). The server computes
// each class's mode from the gate's own inputs (GET /api/v1/trust rows'
// mode and mode_because); the page only names it. Least autonomy first.
export const MODES = ['observes', 'asks_first', 'acts_then_tells', 'acts_in_window']

const MODE_SENTENCE = {
  observes: 'watches only',
  asks_first: 'asks first',
  acts_then_tells: 'acts, then tells you',
  acts_in_window: 'acts in the window',
}

const MODE_COUNT_LABEL = {
  observes: 'watch only',
  asks_first: 'ask first',
  acts_then_tells: 'act, then tell',
  acts_in_window: 'act in window',
}

// normalizeMode reads a row's mode; anything unknown watches only (fail
// closed: the badge never claims more than the server said).
export function normalizeMode(mode) {
  return MODES.includes(mode) ? mode : 'observes'
}

export function modeSentence(mode) {
  return MODE_SENTENCE[normalizeMode(mode)]
}

// modeCounts counts the classes of every database in a trust view by
// mode; rows without a mode (an older server) are not counted.
export function modeCounts(trust) {
  const counts = Object.fromEntries(MODES.map(m => [m, 0]))
  let total = 0
  for (const view of Array.isArray(trust?.databases) ? trust.databases : []) {
    for (const row of Array.isArray(view?.rows) ? view.rows : []) {
      if (typeof row?.mode !== 'string' || row.mode === '') continue
      counts[normalizeMode(row.mode)] += 1
      total += 1
    }
  }
  return { counts, total }
}

// modeBadgeLabel is the header badge text, "12 ask first · 3 act in
// window", in scale order; null when nothing was counted.
export function modeBadgeLabel(summary) {
  if (!summary?.total) return null
  return MODES.filter(m => summary.counts[m] > 0)
    .map(m => `${summary.counts[m]} ${MODE_COUNT_LABEL[m]}`).join(' · ')
}

// stoppedRows names the fleet rows whose emergency stop is on: they run
// nothing, whatever a cached mode says (rowAutoFamilies returns []).
export function stoppedRows(rows) {
  return (Array.isArray(rows) ? rows : [])
    .filter(row => row?.emergency_stopped === true).map(row => row.name)
}
