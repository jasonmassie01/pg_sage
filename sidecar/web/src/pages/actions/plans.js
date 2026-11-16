// plans.js: the plan review's rules in words (ux-03), over plan-03a's
// GET /api/v1/plans/{id} payload (changeplan.Plan, Step and StepCoverage)
// and ux-02a's outcome ledger rows. Pure functions; the components render.

import { ledgerTierLabel } from './LadderSection'
import { VERDICT_LABELS } from '../../lib/outcomes'

const PLAN_ID = /^[1-9][0-9]{0,18}$/
const BIGINT_MAX = '9223372036854775807'
// The outcome ledger's widest window (outcomes.MaxSince, 400 days).
const MAX_SINCE_HOURS = 9600

export const PLAN_STATUS = Object.freeze({
  draft: 'Draft', proposed: 'Awaiting approval', approved: 'Approved', running: 'Running',
  halted: 'Halted', completed: 'Completed', reverted: 'Reverted', rejected: 'Rejected',
  voided: 'Voided', cancelled: 'Cancelled',
})

const STAGE_STATE = Object.freeze({
  pending: 'Not started', starting: 'Starting', queued: 'Queued', running: 'Running',
  executed: 'Ran, waiting for a verdict', judging: 'Judging', reverting: 'Reverting',
  reverted: 'Reverted', skipped: 'Skipped', failed: 'Failed',
  awaiting_operator: 'Waiting for you to run it', halted: 'Halted here',
  awaiting_approval: 'Waiting for its own approval',
})

const METRICS = Object.freeze({ blocks_per_call: 'blocks per call',
  time_per_call: 'time per call', wal_per_tuple: 'WAL per row' })

// expected_source is the ladder rung a step's expected size came from.
const SOURCE_TIERS = Object.freeze({ hypopg: 'plan_only', clone: 'clone', canary: 'canary' })

const ORIGINS = Object.freeze({ generator: 'the plan generator', model: 'a model',
  person: 'a person', agent: 'an agent' })

const NOT_STARTED = new Set(['pending', 'awaiting_approval', 'skipped', 'starting', 'queued'])

export function planHref(id, database) {
  const params = new URLSearchParams({ tab: 'plans', plan: String(id) })
  if (database) params.set('database', database)
  return `#/actions?${params}`
}

// planParam is the hash's plan= id when it is a positive bigint, else ''.
export function planParam(hash = window.location.hash) {
  const id = new URLSearchParams(String(hash || '').split('?')[1] || '').get('plan') || ''
  if (!PLAN_ID.test(id)) return ''
  return id.length === BIGINT_MAX.length && id > BIGINT_MAX ? '' : id
}

// planDatabaseParam is the hash's database= name, or ''.
export function planDatabaseParam(hash = window.location.hash) {
  return new URLSearchParams(String(hash || '').split('?')[1] || '').get('database') || ''
}

export function sortedSteps(plan) {
  const steps = Array.isArray(plan?.steps) ? plan.steps : []
  return [...steps].sort((a, b) => a.ordinal - b.ordinal)
}

// upToChoices are the stages "approve up to stage N" may name: before the
// first never-automatic stage, and before the last (up to the last is all).
export function upToChoices(steps, coverage) {
  const list = Array.isArray(steps) ? [...steps].sort((a, b) => a.ordinal - b.ordinal) : []
  const never = new Set((coverage || []).filter(c => c.needs_own_approval).map(c => c.ordinal))
  const out = []
  for (const s of list.slice(0, -1)) {
    if (never.has(s.ordinal)) break
    out.push(s.ordinal)
  }
  return out
}

// dependants are the stages that depend on stage ordinal through the
// plan's edges, directly or not (plan-02c's dependency rule).
export function dependants(edges, ordinal) {
  const seen = new Set()
  const queue = [ordinal]
  while (queue.length) {
    const n = queue.shift()
    for (const e of edges || []) {
      if (e.from === n && !seen.has(e.to) && e.to !== ordinal) {
        seen.add(e.to)
        queue.push(e.to)
      }
    }
  }
  return [...seen].sort((a, b) => a - b)
}

// haltSummary is a halted plan's halted stage and why, and which stages
// were reverted, kept and never started; null for any other plan.
export function haltSummary(plan) {
  if (plan?.status !== 'halted') return null
  const steps = sortedSteps(plan)
  // A revert_to_checkpoint walk also reverts the kept stages before the
  // one that regressed, so the last reverted stage is where it halted.
  const at = steps.find(s => s.runtime?.state === 'halted')
    || steps.find(s => s.runtime?.state === 'failed')
    || [...steps].reverse().find(s => s.runtime?.state === 'reverted')
  const of = states => steps.filter(s => states(s.runtime?.state || 'pending'))
    .map(s => s.ordinal)
  return { halted: at ? at.ordinal : null, reason: at?.runtime?.state_reason || '',
    reverted: of(st => st === 'reverted'), kept: of(st => st === 'kept'),
    notStarted: of(st => NOT_STARTED.has(st)) }
}

export function stageStatus(runtime) {
  const state = runtime?.state || 'pending'
  if (state === 'kept') {
    if (runtime.verdict === 'improved') return 'Verified: kept'
    if (runtime.verdict === 'no_change') return 'Kept: no change'
    return 'Kept'
  }
  return STAGE_STATE[state] || state
}

function metricName(metric) {
  return METRICS[metric] || String(metric || 'the metric').replaceAll('_', ' ')
}

export function expectedText(step) {
  if (step.metric === 'readback') return 'setting reads back as set'
  const dir = step.expected_direction === 'up' ? 'up' : 'down'
  if (step.expected_direction === 'none') return `${metricName(step.metric)} unchanged`
  const size = typeof step.expected_pct === 'number' ? `${step.expected_pct}%`
    : '(size not estimated)'
  return `${metricName(step.metric)} ${dir} ${size}`
}

// measuredText is a stage's measured effect; unreadable is the ledger's
// read error, said instead of "not measured yet" when there is no row.
export function measuredText(outcome, unreadable = '') {
  const m = outcome?.measured
  if (m && typeof m.change_pct === 'number') {
    const pct = Math.round(m.change_pct * 10) / 10
    return `${metricName(m.metric)} ${pct > 0 ? '+' : ''}${pct}%`
  }
  if (!outcome && unreadable) return `could not be read: ${unreadable}`
  return outcome?.measured_reason ? `not measured yet: ${outcome.measured_reason}`
    : 'not measured yet'
}

export function verdictText(verdict) {
  return VERDICT_LABELS[verdict] || verdict
}

// tierText is the ladder tier a stage was tested at: the ledger row's
// highest passed tier once it ran (lab-03a), else its expected source.
export function tierText(step, outcome) {
  const tier = ledgerTierLabel(outcome?.tier) || ledgerTierLabel(SOURCE_TIERS[step.expected_source])
  return tier || 'Not tested before production'
}

// gateText is what lets the next stage start: next is the next stage's
// coverage, or null for the last stage.
export function gateText(step, next) {
  if (!next) return 'Last stage: the plan completes once it is judged'
  if (next.needs_own_approval || (!next.covered && next.reason)) {
    return `A person approves the next stage: ${next.reason || 'it needs its own approval'}`
  }
  const regressed = step.on_regressed === 'revert_to_checkpoint'
    ? 'reverts to the checkpoint' : 'reverts this stage'
  return 'Automatic: the next stage starts once this one is judged improved or no change; '
    + `a regression ${regressed} and halts the plan`
}

export function originText(plan) {
  const who = ORIGINS[plan.origin] || plan.origin || 'unknown'
  const parts = [plan.model ? `${who} (${plan.model})` : who]
  if (plan.origin_ref) parts.push(plan.origin_ref)
  return `${parts.join(', ')}${plan.created_by ? `, by ${plan.created_by}` : ''}`
}

export function stagesText(list) {
  return list.length === 1 ? `stage ${list[0]}` : `stages ${list.join(', ')}`
}

// outcomesSince is the ledger window (in hours) that covers a plan made
// at createdAt: at least 1h, at most the ledger's 9600h.
export function outcomesSince(createdAt, now = Date.now()) {
  const made = Date.parse(createdAt)
  if (Number.isNaN(made)) return `${MAX_SINCE_HOURS}h`
  const hours = Math.max(1, Math.ceil((now - made) / 3_600_000) + 1)
  return `${Math.min(hours, MAX_SINCE_HOURS)}h`
}

// postPlanDecision POSTs body as JSON and returns {ok, code, message}: the
// server's refusal code (content_changed for a stale hash) and error text.
export async function postPlanDecision(url, body) {
  const res = await fetch(url, { method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
  if (res.ok) return { ok: true, code: '', message: '' }
  let reply = null
  try {
    reply = JSON.parse(await res.text())
  } catch (err) {
    if (!(err instanceof SyntaxError)) throw err
  }
  const text = typeof reply?.error === 'string' ? reply.error : 'Request failed'
  return { ok: false, code: typeof reply?.code === 'string' ? reply.code : '',
    message: `${text} (${res.status})` }
}
