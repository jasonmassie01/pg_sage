// facts.ts: binding-fact payloads for facts-dialogs.spec.ts (facts-07
// CHECK-03: the revoke and expire confirmation wording). Each payload names
// the Go code whose JSON it copies. Register these routes AFTER
// mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

// confirmedFact copies facts.Fact as GET /api/v1/facts lists it for a
// confirmed slot_consumer (the fixture of FactCardConfirm.test.jsx and
// TestFactsDecide* in internal/api): summary and provenance are server text.
export function confirmedFact(id: number, extra: Record<string, unknown> = {}) {
  return {
    id, database: 'orders', type: 'slot_consumer', subject_kind: 'slot',
    subject: 'cdc_orders', value: { consumer: 'debezium' }, source: 'operator',
    proposed_by: 'alice@example.com', evidence: [], rationale: '', status: 'confirmed',
    decided_by: 'alice@example.com', decided_at: '2026-10-01T10:00:00Z',
    decision_note: '', expires_at: null, expired_reason: '', proposals: 1,
    summary: 'slot cdc_orders belongs to debezium',
    provenance: `fact #${id}, confirmed by alice@example.com on 2026-10-01`, ...extra,
  }
}

// STATUS_AFTER is the status each decide verb leaves (facts.Store.Decide).
const STATUS_AFTER: Record<string, string> = {
  confirm: 'confirmed', reject: 'rejected', expire: 'expired',
}

export type Decision = { url: string, body: unknown }
export type DecideAnswer = { status: number, json: unknown } | null

// routeFacts serves facts (filtered by ?status=) and applies each POST
// /api/v1/facts/{id}/{verb}; answer may refuse one (the state is unchanged).
export async function routeFacts(page: Page, facts: Record<string, unknown>[],
  answer: () => DecideAnswer = () => null) {
  const decisions: Decision[] = []
  await page.route(url => url.pathname === '/api/v1/facts', route => {
    const status = new URL(route.request().url()).searchParams.get('status')
    const listed = status ? facts.filter(f => f.status === status) : facts
    return route.fulfill({ json: { facts: listed, errors: [] } })
  })
  await page.route(url => /^\/api\/v1\/facts\/\d+\/\w+$/.test(url.pathname), route => {
    const url = new URL(route.request().url())
    decisions.push({ url: url.pathname + url.search, body: route.request().postDataJSON() })
    const refused = answer()
    if (refused) return route.fulfill(refused)
    const [, , , , id, verb] = url.pathname.split('/')
    const fact = facts.find(f => f.id === Number(id))
    if (fact) fact.status = STATUS_AFTER[verb]
    return route.fulfill({ json: fact })
  })
  await page.route(url => url.pathname === '/api/v1/facts/coverage', route =>
    route.fulfill({ json: { objects: ['public.orders'], truncated: false } }))
  return decisions
}
