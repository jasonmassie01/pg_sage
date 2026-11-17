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

// evid-03b (facts-owner-brief.spec.ts). ownerBrief copies facts.Fact for a
// confirmed owner_brief as GET /api/v1/facts lists it (fact_routes.go
// viewOf); peakSuggestion copies the JSON of GET /api/v1/databases/{db}/
// facts/peak-suggestion (fact_peak_suggestion.go).
export function ownerBrief(id: number, database: string, value: Record<string, string>) {
  return {
    id, database, type: 'owner_brief', subject_kind: 'database', subject: '*', value,
    source: 'operator', proposed_by: 'alice@example.com', evidence: [], rationale: '',
    status: 'confirmed', decided_by: 'alice@example.com',
    decided_at: '2026-11-01T10:00:00Z', decision_note: '', expires_at: null,
    expired_reason: '', proposals: 1,
    summary: 'owner brief: ' + Object.entries(value).map(([k, v]) => `${k} ${v}`).join(', '),
    provenance: `fact #${id}, confirmed by alice@example.com on 2026-11-01`,
  }
}

export function peakSuggestion(database: string, peak: string | null, reason = '') {
  const window = { from: '2026-10-19T00:00:00Z', to: '2026-11-16T00:00:00Z' }
  if (!peak) {
    return { database, suggestion: 'none', reason, coverage_status: 'learning', hours: [],
      ...window }
  }
  return { database, suggestion: 'peak_windows', peak_windows: peak, coverage_status: 'ok',
    ...window, hours: [{ hour: '2026-10-19T09:00:00Z', exec_ms: 10009 },
      { hour: '2026-10-26T09:00:00Z', exec_ms: 10009 },
      { hour: '2026-11-02T09:00:00Z', exec_ms: 10009 }] }
}

export type Declaration = { url: string, body: Record<string, unknown> }

// routeDeclarations serves GET /api/v1/facts (by ?database=, ?status= and
// ?type=), each database's peak suggestion and POST /api/v1/facts, which
// confirms a declaration as facts.Store.Declare does: an owner brief is
// replaced in place, any other fact is added. It returns what was sent.
export async function routeDeclarations(page: Page, facts: Record<string, unknown>[],
  suggestion: (database: string) => unknown) {
  const declarations: Declaration[] = []
  await page.route(url => url.pathname === '/api/v1/facts', route => {
    const url = new URL(route.request().url())
    const database = url.searchParams.get('database') || 'all'
    if (route.request().method() === 'POST') {
      const body = route.request().postDataJSON()
      declarations.push({ url: url.pathname + url.search, body })
      return route.fulfill({ status: 201, json: { fact: declaredFact(facts, database, body) } })
    }
    const status = url.searchParams.get('status')
    const type = url.searchParams.get('type')
    const listed = facts.filter(f => (!status || f.status === status) &&
      (!type || f.type === type) && (database === 'all' || f.database === database))
    return route.fulfill({ json: { facts: listed, errors: [] } })
  })
  await page.route(url => /^\/api\/v1\/databases\/[^/]+\/facts\/peak-suggestion$/
    .test(url.pathname), route => {
    const database = new URL(route.request().url()).pathname.split('/')[4]
    return route.fulfill({ json: suggestion(database) })
  })
  await page.route(url => url.pathname === '/api/v1/facts/coverage', route =>
    route.fulfill({ json: { objects: ['public.orders'], truncated: false } }))
  return declarations
}

function declaredFact(facts: Record<string, unknown>[], database: string,
  body: Record<string, unknown>) {
  const value = (body.value || {}) as Record<string, string>
  if (body.type === 'owner_brief') {
    const prior = facts.findIndex(f => f.type === 'owner_brief' && f.database === database)
    const id = prior >= 0 ? Number(facts[prior].id) : 100 + facts.length
    const fact = ownerBrief(id, database, value)
    if (prior >= 0) facts[prior] = fact
    else facts.push(fact)
    return fact
  }
  const fact = { ...confirmedFact(100 + facts.length), database, type: body.type,
    subject_kind: body.subject_kind, subject: body.subject, value,
    summary: `${body.subject_kind} ${body.subject} must not be touched` }
  facts.push(fact)
  return fact
}
