// plans.ts: plan payloads for ux-03's plan review specs (plan-03a). The
// shapes copy the handler golden internal/api/testdata/plan_detail.golden.json
// (TestPlanDetail_MatchesTheGolden): when the golden changes, change these.
// Register these routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

export const PLAN_HASH = '4f1c2b0d9e8a7f6e5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3b2a19087f6e'

// planStep copies changeplan.Step as the detail route serves it.
export function planStep(id: number, ordinal: number, name: string,
  extra: Record<string, unknown> = {}) {
  return {
    action_type: 'create_index_concurrently', checkpoint: false, deadline_minutes: 60,
    expected_direction: 'down', expected_source: 'hypopg', finding_id: 11, id,
    kind: 'action', metric: 'blocks_per_call', min_evidence: {}, on_expired: 'halt_ask',
    on_improved: 'keep', on_inconclusive: 'halt_ask', on_no_change: 'keep',
    on_regressed: 'revert_step', ordinal,
    proposed_sql: `CREATE INDEX CONCURRENTLY ${name} ON public.orders (customer_id)`,
    rollback_sql: `DROP INDEX CONCURRENTLY public.${name}`, runtime: { state: 'pending' },
    target_queryids: [101], ...extra,
  }
}

// planDetail is GET /api/v1/plans/{id}: a three-step plan approved up to
// step 2, so step 3 is not covered.
export function planDetail(extra: Record<string, unknown> = {}) {
  return {
    approvals: [{
      approver_user_id: 2, decided_at: '2026-11-16T09:30:00Z', id: 41, mode: 'up_to',
      plan_hash: PLAN_HASH, plan_id: 7, up_to: 2, via: 'ui',
    }],
    coverage: [
      { by: 'plan', covered: true, needs_own_approval: false, ordinal: 1, step_id: 70 },
      { by: 'plan', covered: true, needs_own_approval: false, ordinal: 2, step_id: 71 },
      { covered: false, needs_own_approval: false, ordinal: 3,
        reason: 'the plan was approved up to step 2', step_id: 72 },
    ],
    database: 'orders',
    plan: {
      created_at: '2026-11-16T09:30:00Z', created_by: 'alice',
      edges: [{ from: 1, to: 2 }, { from: 2, to: 3 }],
      goal: 'cut blocks per call on the order lookups', id: 7, origin: 'person',
      plan_hash: PLAN_HASH,
      scope: { objects: ['public.orders'], queryids: [101] },
      status: 'approved',
      steps: [planStep(70, 1, 'idx_a'), planStep(71, 2, 'idx_b'), planStep(72, 3, 'idx_c')],
      updated_at: '2026-11-16T09:30:00Z',
    },
    plan_hash: PLAN_HASH,
    step_approvals: [],
    ...extra,
  }
}

// planList is GET /api/v1/plans: each plan without steps, with its database.
export function planList() {
  const { plan, database } = planDetail()
  return { plans: [{ database, ...plan, steps: null, edges: null }], total: 1, errors: [] }
}

// The decision answers (plan_decisions.go): success and the refusals ux-03
// shows.
export const approveAnswer = {
  ok: true, database: 'orders', plan_id: 7, status: 'approved',
  approval: { id: 42, plan_id: 7, plan_hash: PLAN_HASH, approver_user_id: 2, mode: 'all',
    via: 'ui', decided_at: '2026-11-16T09:31:00Z' },
}
export const contentChanged = {
  error: 'the plan changed after it was shown; review it again', code: 'content_changed',
}
export const notPending = {
  error: 'the plan is not awaiting approval in its status', code: 'not_pending',
}

export type PlanDecision = { url: string, body: unknown }
export type PlanAnswer = { status: number, json: unknown } | null

// routePlans serves the list and detail and records each decision POST;
// answer may refuse one (the default approves). detail may be a function,
// called for each GET, so a spec can move the plan on between polls (ux-03).
export async function routePlans(page: Page,
  detail: unknown | (() => unknown) = planDetail(),
  answer: () => PlanAnswer = () => null) {
  const decisions: PlanDecision[] = []
  await page.route(url => url.pathname === '/api/v1/plans',
    route => route.fulfill({ json: planList() }))
  await page.route(url => /^\/api\/v1\/plans\/\d+(\/.*)?$/.test(url.pathname), route => {
    const req = route.request()
    if (req.method() === 'GET') {
      return route.fulfill({ json: typeof detail === 'function' ? detail() : detail })
    }
    decisions.push({ url: req.url(), body: req.postDataJSON() })
    const refused = answer()
    if (refused) return route.fulfill({ status: refused.status, json: refused.json })
    return route.fulfill({ json: approveAnswer })
  })
  return decisions
}

// fivePlan is ux-03's halted-plan story: a proposed five-stage plan with
// no approval; withRuntimes moves it on (plan status and stage runtimes).
export function fivePlan() {
  const steps = [1, 2, 3, 4, 5].map(n => planStep(69 + n, n, `idx_${n}`))
  return planDetail({
    approvals: [],
    coverage: steps.map(s => ({ covered: false, needs_own_approval: false,
      ordinal: s.ordinal, reason: 'the plan has no live approval at its current hash',
      step_id: s.id })),
    plan: { ...planDetail().plan, status: 'proposed', steps,
      edges: [1, 2, 3, 4].map(n => ({ from: n, to: n + 1 })) },
  })
}

export function withRuntimes(detail: ReturnType<typeof planDetail>, status: string,
  runtimes: Record<number, Record<string, unknown>>) {
  const steps = detail.plan.steps.map(s => ({ ...s,
    runtime: { ...s.runtime, ...(runtimes[s.ordinal] || {}) } }))
  return { ...detail, plan: { ...detail.plan, status, steps } }
}
