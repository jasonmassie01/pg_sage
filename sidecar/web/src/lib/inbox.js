// inbox.js: the wording and arithmetic of the Inbox (ux-01b), kept out of
// the components so the badge and the page say the same thing.

export const SURFACE_HEADER = 'X-Sage-Surface'

// KIND_LABELS names ux-01a's inbox kinds: one item, and a source of many.
export const KIND_LABELS = Object.freeze({
  change_approval: ['Change approval', 'change approvals'],
  plan_approval: ['Plan approval', 'plan approvals'],
  autonomy_promotion: ['Autonomy promotion', 'autonomy promotions'],
  fact: ['Fact', 'facts'],
  runbook_signature: ['Runbook signature', 'runbook signatures'],
  agent_request: ['Agent request', 'agent requests'],
  policy_exception: ['Policy exception', 'policy exceptions'],
})

export const kindLabel = kind => KIND_LABELS[kind]?.[0] || String(kind || 'Item')
const sourceLabel = kind => KIND_LABELS[kind]?.[1] || String(kind || 'unknown source')

// BADGE_CAP: above it the badge reads "99+".
const BADGE_CAP = 99

function sourceErrors(errors) {
  if (!Array.isArray(errors)) return []
  return errors.map(e => {
    const where = e?.database ? ` (${e.database})` : ''
    return { text: `${sourceLabel(e?.source)}${where}`, error: e?.error || 'not read' }
  })
}

// inboxBadge turns /api/v1/inbox/count into the nav badge, or null. A
// count that missed a source is a lower bound, shown "N+"; above 99 the
// badge is "99+". A missing or malformed total shows nothing.
export function inboxBadge(data) {
  const total = data?.total
  if (!Number.isInteger(total) || total < 0) return null
  const lower = data.lower_bound === true
  if (total === 0 && !lower) return null
  if (total > BADGE_CAP) {
    return { text: `${BADGE_CAP}+`, title: lower ? `At least ${total} items need you`
      : `${total} items need you` }
  }
  if (!lower) return { text: String(total), title: undefined }
  const missed = sourceErrors(data.errors).map(e => `${e.text}: ${e.error}`).join(', ')
  return { text: `${total}+`, title: `At least ${total}; not read: ${missed || 'unknown'}` }
}

// sourceErrorText is "Could not read: facts, autonomy promotions (orders)",
// one name per failed source, or '' when every source answered.
export function sourceErrorText(errors) {
  const names = [...new Set(sourceErrors(errors).map(e => e.text))]
  return names.length ? `Could not read: ${names.join(', ')}` : ''
}

// FIRST_VISIT_SINCE is the ledger window of a first visit.
const FIRST_VISIT_SINCE = '24h'
const DAY_MS = 24 * 3600 * 1000
// MAX_SINCE_S is the ledger's longest window (outcomes.MaxSince, 9600 h).
const MAX_SINCE_S = 9600 * 3600
// SLACK_S widens the window, fixed when the page opens, so a change at
// the seen time stays in it while the page polls for an hour (the server
// counts the window back from its own now); rows are then filtered at the
// seen time.
const SLACK_S = 3600

// sinceWindow is the ledger read for "Since you were last here": seenAt
// (an RFC 3339 string, or null on a first visit) to now.
export function sinceWindow(seenAt, now = Date.now()) {
  const seen = seenAt ? Date.parse(seenAt) : NaN
  if (!Number.isFinite(seen)) {
    return { since: FIRST_VISIT_SINCE, from: now - DAY_MS, firstVisit: true }
  }
  const seconds = Math.max(0, Math.ceil((now - seen) / 1000)) + SLACK_S
  return { since: `${Math.min(seconds, MAX_SINCE_S)}s`, from: seen, firstVisit: false }
}

// changesSince keeps the ledger rows executed at or after from, once each.
export function changesSince(rows, from) {
  if (!Array.isArray(rows)) return []
  const seen = new Set()
  return rows.filter(r => {
    const key = `${r?.database}:${r?.action_id}`
    const at = Date.parse(r?.executed_at)
    if (seen.has(key) || !Number.isFinite(at) || at < from) return false
    seen.add(key)
    return true
  })
}

const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday',
  'Saturday']
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct',
  'Nov', 'Dec']

// sinceLabel words a seen time against now (local time): "since 09:30"
// today, "since Tuesday" within the week, else "since Nov 3".
export function sinceLabel(from, now = Date.now()) {
  const d = new Date(from)
  const today = new Date(now)
  if (d.toDateString() === today.toDateString()) {
    return `since ${String(d.getHours()).padStart(2, '0')}:`
      + `${String(d.getMinutes()).padStart(2, '0')}`
  }
  if (now - from < 7 * DAY_MS) return `since ${WEEKDAYS[d.getDay()]}`
  return `since ${MONTHS[d.getMonth()]} ${d.getDate()}`
}

// MEASURED: the verdicts judged on a measurement (outcomes.Summarize).
const MEASURED = new Set(['improved', 'no_change', 'regressed'])

// goalSentence is the empty state, built from counts: "Nothing needs you."
// and, when changes ran, "6 changes since Tuesday, all verified." A partial
// count (see ledgerGaps) is "At least 6" and never claims "all verified".
export function goalSentence(changes, when, partial = false) {
  if (!changes.length) return 'Nothing needs you.'
  const verified = changes.filter(r => MEASURED.has(r.verdict)).length
  const noun = changes.length === 1 ? 'change' : 'changes'
  const count = `${partial ? 'At least ' : ''}${changes.length} ${noun}`
  const tail = verified === changes.length && !partial ? 'all verified' : `${verified} verified`
  return `Nothing needs you. ${count} ${when}, ${tail}.`
}

// LEDGER_PAGE is the outcome ledger's default page (outcomes.DefaultLimit).
export const LEDGER_PAGE = 500

// ledgerGaps says whether a ledger read since from may miss changes: a
// database that could not be read (failed, "name (error)"), or a page cut
// short (cut: the server truncated, or a full page whose oldest row is
// still at or after from, so older changes since from may be off it).
export function ledgerGaps(data, from) {
  const rows = Array.isArray(data?.rows) ? data.rows : []
  const failed = (Array.isArray(data?.errors) ? data.errors : [])
    .map(e => `${e?.database || 'unknown'} (${e?.error || 'not read'})`)
  const oldest = rows.length ? Date.parse(rows[rows.length - 1]?.executed_at) : NaN
  const full = rows.length >= LEDGER_PAGE && !(oldest < from)
  const cut = data?.truncated === true || full
  return { partial: cut || failed.length > 0, failed, cut }
}

// withSurface adds X-Sage-Surface to a request's headers (ux-01a's metric).
export function withSurface(headers, surface) {
  return surface ? { ...headers, [SURFACE_HEADER]: surface } : headers
}
