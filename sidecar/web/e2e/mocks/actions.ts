// actions.ts: Actions page payloads and routes for the dashboard-14 specs.
// Every payload names the Go test (or struct) whose JSON it copies, so a
// renamed field on the server shows up as a failing spec here. Register
// these routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page, type Request } from '@playwright/test'

/* ---------- Ledger rows (GET /api/v1/actions) ---------- */

// executedRow copies an executed sage.action_log ledger row as
// TestActionsLedger_RecordKindDistinguishesQueueAndLog reads it (record_kind
// "executed", ledger_key "log:N"); the rollback fields are those
// executor-07's RollbackAction leaves on a withheld rollback.
export function executedRow(overrides: Record<string, unknown> = {}) {
  return {
    id: '9', ledger_key: 'log:9', record_kind: 'executed',
    action_type: 'create_index', outcome: 'rollback_skipped', risk_tier: 'moderate',
    database_name: 'orders',
    sql_executed: 'CREATE INDEX CONCURRENTLY idx_s ON public.orders(customer_id)',
    rollback_sql: 'DROP INDEX CONCURRENTLY IF EXISTS idx_s',
    rollback_reason: 'emergency stop active; automatic rollback withheld',
    executed_at: '2026-11-12T09:00:00Z', verification_status: 'verified',
    ...overrides,
  }
}

// The body RollbackControl.postRollback sends: since ux-01b the reason is
// the one the operator types (required, 3 to 500 characters) instead of a
// fixed string; rollbackActionHandler reads "reason" and the ledger kind
// guard reads "record_kind".
export const rollbackReason = 'checkout writes slowed down'
export const rollbackBody = { reason: rollbackReason, record_kind: 'executed' }

/* ---------- Scheduled approvals (GET /api/v1/actions/pending) ---------- */

// scheduledRow copies the "scheduled" entry TestPendingListShowsScheduledApprovals
// reads: status scheduled, run_after, database_name and scheduled_by.
export function scheduledRow(overrides: Record<string, unknown> = {}) {
  return {
    id: 41, database_name: 'orders', status: 'scheduled', action_risk: 'moderate',
    proposed_sql: 'CREATE INDEX CONCURRENTLY i ON public.t (c)',
    run_after: '2026-11-17T07:00:00Z', window_closes_at: '2026-11-17T11:00:00Z',
    scheduled_by: 2,
    ...overrides,
  }
}

// pendingQueued is the same queue row after Cancel returned it to pending.
export function pendingQueued() {
  return {
    id: 41, action_type: 'create_index', action_risk: 'moderate', status: 'pending',
    finding_id: 42, database_name: 'orders',
    proposed_sql: 'CREATE INDEX CONCURRENTLY i ON public.t (c)',
    proposed_at: '2026-11-12T08:00:00Z', expires_at: '2026-11-19T08:00:00Z',
  }
}

export type Posted = { url: string, body: unknown }

export type ActionsState = {
  ledger: () => unknown[]
  pending?: () => { pending: unknown[], scheduled?: unknown[] }
  // rollback answers a POST .../rollback; the default is the handler's 200.
  rollback?: (req: Request) => { status: number, json: unknown }
  unschedule?: (req: Request) => { status: number, json: unknown }
}

function postedJSON(req: Request) {
  try {
    return req.postDataJSON()
  } catch {
    return req.postData()
  }
}

const rolledBackOK = (id: string) => ({
  status: 200, json: { ok: true, action_id: Number(id), status: 'rolled_back' },
})

// routeActions answers the ledger, the pending list and its count, rollback
// and unschedule; every POST is recorded in posts, in order.
export async function routeActions(page: Page, state: ActionsState) {
  const posts: Posted[] = []
  await page.route('**/api/v1/actions**', route => {
    const req = route.request()
    const url = new URL(req.url())
    if (req.method() === 'POST') {
      posts.push({ url: url.pathname + url.search, body: postedJSON(req) })
      const id = url.pathname.split('/')[4]
      if (url.pathname.endsWith('/rollback')) {
        return route.fulfill(state.rollback ? state.rollback(req) : rolledBackOK(id))
      }
      if (url.pathname.endsWith('/unschedule')) {
        return route.fulfill(state.unschedule ? state.unschedule(req)
          : { status: 200, json: { ok: true, status: 'pending' } })
      }
      return route.fulfill({ status: 404, json: { error: 'not mocked' } })
    }
    const pending = state.pending ? state.pending() : { pending: [], scheduled: [] }
    if (url.pathname === '/api/v1/actions/pending/count') {
      return route.fulfill({ json: { count: pending.pending.length } })
    }
    if (url.pathname === '/api/v1/actions/pending') {
      return route.fulfill({ json: { ...pending, total: pending.pending.length } })
    }
    const rows = state.ledger()
    return route.fulfill({ json: { actions: rows, total: rows.length } })
  })
  return posts
}

/* ---------- Decisions (GET /api/v1/decisions) ---------- */

// budgetPark copies the entry TestDecisionsListsEntriesWithHumanReason
// serves: a parked budget decision with its human reason and release time.
export const budgetPark = {
  database: 'orders', decision_id: 7, candidate_key: 'ck_7', verdict: 'parked',
  reason: 'rate_limit_exceeded', human_reason: 'rate limit exceeded (next frees at 18:00)',
  risk_tier: 'safe', feature: 'index', target_objects: ['public.orders'],
  repeat_count: 3, open_rows: 1, first_seen: '2026-11-12T09:00:00Z',
  last_seen_at: '2026-11-12T10:00:00Z', budget_detail: 'next frees at 18:00',
  policy_version: 3, policy_doc_sha256: 'cd'.repeat(32),
}

// windowBlock is a non-budget decision: it carries no budget_detail.
export const windowBlock = {
  ...budgetPark, decision_id: 8, candidate_key: 'ck_8', verdict: 'blocked',
  reason: 'outside_maintenance_window', human_reason: 'outside maintenance window',
  budget_detail: undefined, repeat_count: 1, queue_id: 9,
}

export function decisionsBody(decisions: unknown[]) {
  return { database: 'all', decisions, partial: [], truncated: false, limit: 100 }
}

// routeDecisions answers GET /api/v1/decisions and records each URL asked.
export async function routeDecisions(page: Page,
  answer: (url: URL) => { status: number, json: unknown }) {
  const asked: string[] = []
  await page.route('**/api/v1/decisions**', route => {
    const url = new URL(route.request().url())
    asked.push(url.pathname + url.search)
    return route.fulfill(answer(url))
  })
  return asked
}

/* ---------- MCP tokens (GET/DELETE /api/v1/mcp/tokens) ---------- */

// operatorToken copies activeOperator from src/pages/mcptokens/testStub.js,
// which answers the token API the way the sidecar does (no plaintext).
export function operatorToken(revoked = false) {
  return {
    id: 'a1b2c3d4-0000-4000-8000-000000000002', name: 'oncall-cursor',
    kind: 'operator', scopes: ['read', 'propose', 'approve'], databases: ['*'],
    owner_user_id: 5, created_by: 'creator@x.test', created_at: '2026-11-08T09:00:00Z',
    expires_at: '2027-01-01T00:00:00Z', last_used_at: null, prefix: 'sage_mcp_OpR',
    ...(revoked ? { revoked_at: '2026-11-13T09:00:00Z', revoked_by: 'test@test.com' } : {}),
  }
}

// routeTokens lists one operator token and answers its DELETE with the
// revoked token; deletes records each DELETE path.
export async function routeTokens(page: Page) {
  const deletes: string[] = []
  let revoked = false
  await page.route('**/api/v1/users**', route => route.fulfill({
    json: { users: [{ id: 5, email: 'oncall@x.test', role: 'operator' }] },
  }))
  await page.route('**/api/v1/mcp/tokens**', route => {
    const req = route.request()
    if (req.method() === 'DELETE') {
      deletes.push(new URL(req.url()).pathname)
      revoked = true
      return route.fulfill({ json: operatorToken(true) })
    }
    return route.fulfill({ json: { tokens: [operatorToken(revoked)] } })
  })
  return deletes
}
