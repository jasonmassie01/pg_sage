// planTestData: plan payloads for ux-03's unit tests, in the shapes
// GET /api/v1/plans/{id} serves (internal/api/testdata/plan_detail.golden.json,
// copied by e2e/mocks/plans.ts). Only the tests import it.

export const PLAN_HASH = '4f1c2b0d9e8a7f6e5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3b2a19087f6e'

export function planStep(id, ordinal, name, extra = {}) {
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

export function coverageFor(steps, extra = {}) {
  return steps.map(s => ({ covered: false, needs_own_approval: false, ordinal: s.ordinal,
    reason: 'the plan has no live approval at its current hash', step_id: s.id,
    ...(extra[s.ordinal] || {}) }))
}

// proposedPlan is a three-stage plan awaiting approval, with no approval.
export function proposedPlan({ steps, coverage, plan = {}, ...extra } = {}) {
  const planSteps = steps || [planStep(70, 1, 'idx_a', { evidence_ids: ['ev-1'] }),
    planStep(71, 2, 'idx_b'), planStep(72, 3, 'idx_c')]
  return {
    approvals: [], coverage: coverage || coverageFor(planSteps), database: 'orders',
    plan: {
      created_at: '2026-11-16T09:30:00Z', created_by: 'alice',
      edges: planSteps.slice(1).map(s => ({ from: s.ordinal - 1, to: s.ordinal })),
      goal: 'cut blocks per call on the order lookups', id: 7, origin: 'person',
      plan_hash: PLAN_HASH, scope: { objects: ['public.orders'], queryids: [101] },
      status: 'proposed', steps: planSteps, updated_at: '2026-11-16T09:30:00Z', ...plan,
    },
    plan_hash: PLAN_HASH, step_approvals: [], ...extra,
  }
}

// withStates sets each stage's runtime (by ordinal) and the plan status.
export function withStates(detail, status, runtimes) {
  const steps = detail.plan.steps.map(s => ({ ...s,
    runtime: { ...s.runtime, ...(runtimes[s.ordinal] || {}) } }))
  return { ...detail, plan: { ...detail.plan, status, steps } }
}

// jsonResponse is a fetch answer with body as JSON.
export function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), { status,
    headers: { 'Content-Type': 'application/json' } })
}
