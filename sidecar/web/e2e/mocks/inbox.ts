// inbox.ts: Inbox payloads (ux-01b) in the shapes the routes serve:
// inbox.Result (internal/inbox/service.go) for GET /api/v1/inbox and its
// /count, {seen_at} for /api/v1/inbox/seen (internal/api/inbox_seen.go),
// an approval card for /api/v1/approvals/{id} and outcomes.Result rows
// for the ledger. Register AFTER mockAllAPIs: Playwright matches the
// newest route first.

import { type Page, type Route } from '@playwright/test'

export const SEEN_AT = '2026-11-10T15:00:00Z'

export function inboxItem(kind: string, id: string, extra: Record<string, unknown> = {}) {
  return {
    kind, id, database: 'primary', title: `${kind} ${id}`, why: `why ${id}`,
    risk: 'moderate', created_at: SEEN_AT, href: `#/actions?tab=pending&queue=${id}`,
    source: 'sage.action_queue', actionable: true, ...extra,
  }
}

export const approvalCard = {
  queue_id: 42, database: 'primary', title: 'Add index on orders(customer_id)',
  action_type: 'create_index', status: 'pending', targets: ['public.orders'], evidence: [],
  predicted_effect: {}, sql: 'CREATE INDEX CONCURRENTLY idx_c ON public.orders (customer_id)',
  rollback: { class: 'drop_index', sql: 'DROP INDEX CONCURRENTLY IF EXISTS idx_c' },
  risk: { tier: 'moderate', guardrails: [], post_checks: ['blocks per call down'] },
  why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
  expires_at: '2030-01-01T00:00:00Z', card_hash: 'h42',
}

export const ledgerRow = {
  action_id: 12, database: 'primary', action_type: 'create_index', what: 'Add an index',
  why: '', origin: 'approved', expected: null, measured: null, verdict: 'improved',
  rollback_state: 'available', executed_at: '2026-11-11T03:00:00Z',
}

export interface Post { url: string, headers: Record<string, string>, body: unknown }

export interface InboxState {
  items: ReturnType<typeof inboxItem>[]
  rows: (typeof ledgerRow)[]
  total?: number
}

function result(state: InboxState) {
  const total = state.total ?? state.items.length
  return { items: state.items, counts_by_kind: {}, total, lower_bound: false,
    truncated: false, errors: [] }
}

function recorder(posts: Post[]) {
  return (route: Route) => {
    const req = route.request()
    const url = new URL(req.url())
    posts.push({ url: url.pathname + url.search, headers: req.headers(),
      body: req.postDataJSON() })
  }
}

// routeInbox serves the Inbox's routes from state and records every POST.
// Approving queue item 42 removes it; rolling back action 12 marks the
// ledger rows rolled back, as the next poll would show.
export async function routeInbox(page: Page, state: InboxState) {
  const posts: Post[] = []
  const record = recorder(posts)
  await page.route(url => url.pathname.startsWith('/api/v1/inbox'), route => {
    const path = new URL(route.request().url()).pathname
    if (path !== '/api/v1/inbox/seen') return route.fulfill({ json: result(state) })
    if (route.request().method() === 'POST') record(route)
    return route.fulfill({ json: { seen_at: SEEN_AT } })
  })
  await page.route(url => url.pathname === '/api/v1/outcomes', route =>
    route.fulfill({ json: { since: '1h', rows: state.rows, summary: {}, truncated: false,
      errors: [] } }))
  await page.route(url => url.pathname.startsWith('/api/v1/approvals/42'), route => {
    if (route.request().method() !== 'POST') {
      return route.fulfill({ json: { card: approvalCard, eligible: true } })
    }
    record(route)
    state.items = state.items.filter(i => i.id !== '42')
    return route.fulfill({ json: { ok: true, status: 'executed', executed: true } })
  })
  await page.route(url => url.pathname.startsWith('/api/v1/actions/12'), route => {
    if (route.request().method() !== 'POST') {
      return route.fulfill({ json: { id: 12, database_name: 'primary',
        record_kind: 'executed', rollback_sql: 'DROP INDEX CONCURRENTLY IF EXISTS idx_c',
        risk_tier: 'moderate' } })
    }
    record(route)
    state.rows = state.rows.map(r => ({ ...r, rollback_state: 'rolled_back' }))
    return route.fulfill({ json: { ok: true, action_id: 12, status: 'rolled_back' } })
  })
  return posts
}
