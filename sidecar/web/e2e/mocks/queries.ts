// queries.ts: Explain and query-page payloads for the dashboard-13 specs.
// Every payload names the Go test (or struct) whose JSON it copies.

import { type Page, type Request } from '@playwright/test'

// explainRulesOnly copies explain.ExplainResult (internal/explain/explain.go)
// as TestExplainHandler_KnownDatabaseRuns returns it with no LLM: the rule
// entries of internal/explain/rules_floor_test.go, each tagged "rules".
export const explainRulesOnly = {
  query: 'SELECT payload FROM orders ORDER BY payload',
  plan_json: null,
  summary: 'Sort; sort spilled 8.2 MB to disk',
  slow_because: ['sort spilled 8.2 MB to disk'],
  recommendations: ['raise work_mem for this statement'],
  insights: [
    { kind: 'slow_because', text: 'sort spilled 8.2 MB to disk', source: 'rules',
      symptom: 'disk_sort' },
    { kind: 'recommendation', text: 'raise work_mem for this statement', source: 'rules',
      symptom: 'disk_sort' },
  ],
  node_breakdown: [
    { node_type: 'Sort', description: 'Sort', time_ms: 30.5, loops: 1, rows: 1000,
      row_estimate: 1000 },
  ],
  estimated_cost: 512.5,
  actual_time_ms: 41.5,
  plan_source: 'explain',
}

// explainFailure is the API's jsonError body for a 500.
export const explainFailure = { error: 'explain failed: canceling statement due to timeout' }

// routeExplain answers POST /api/v1/explain with respond(n), n counting
// from 1, and records each request so a spec can check its body.
export async function routeExplain(page: Page,
  respond: (n: number) => { status: number, json: unknown }) {
  const requests: Request[] = []
  await page.route('**/api/v1/explain**', route => {
    requests.push(route.request())
    return route.fulfill(respond(requests.length))
  })
  return requests
}

// queryDetail copies GET /api/v1/queries/{queryid} as
// TestQueryDetailDB_HistoryPlanAndLinks checks it (query_detail.go):
// history buckets, one plan change p1 -> p2, the cached auto_explain plan
// and the finding and hint about the query.
export function queryDetail(queryid: string) {
  return {
    queryid, database: 'orders', coverage: 'full',
    query: 'SELECT payload FROM orders ORDER BY payload',
    buckets: [
      { start: '2026-11-12T08:00:00Z', calls: 120, total_time_ms: 4920,
        mean_time_ms: 41, rows: 120000, epoch_break: false },
      { start: '2026-11-12T09:00:00Z', calls: 150, total_time_ms: 9300,
        mean_time_ms: 62, rows: 150000, epoch_break: false },
    ],
    plan_changes: [{ at: '2026-11-12T09:00:00Z', from: 'p1', to: 'p2' }],
    plan: {
      source: 'auto_explain', captured_at: '2026-11-12T09:05:00Z', total_cost: 42.5,
      plan_hash: 'p2', node_types: ['Sort', 'Seq Scan'],
      plan_json: [{ Plan: { 'Node Type': 'Sort' } }],
    },
    cases: [{ finding_id: 31, title: 'Slow query', severity: 'warning', status: 'open' }],
    hints: [{ id: 5, hint_text: 'Set(work_mem "64MB")', status: 'active' }],
    actions: [],
  }
}

// callSiteOff copies GET /api/v1/queries/{queryid}/call-site without a
// code host, as TestCallSiteRoute_OffWithoutACodeHost checks it (pr-01a).
export const callSiteOff = {
  configured: false, database: 'orders',
  note: 'No code host is configured: set agents.code_host.provider and ' +
    'agents.code_host.repo, and the read token in SAGE_CODE_HOST_TOKEN.',
}

// callSiteExact copies the exact answer of TestCallSiteRouteDB_ExactCallSiteFromTags.
export const callSiteExact = {
  configured: true, database: 'orders', repo: 'acme/shop', host: 'github api.github.com',
  summary: 'Called from app/models/order.rb:3 (exact)',
  call_site: { path: 'app/models/order.rb', line: 3, confidence: 'exact',
    method: 'sqlcommenter file and line' },
}

// routeQueryDetail answers the query page for any queryid, and its call
// site with callSite, and records the URLs asked for.
export async function routeQueryDetail(page: Page, callSite: unknown = callSiteOff) {
  const urls: string[] = []
  await page.route('**/api/v1/queries/**', route => {
    const url = new URL(route.request().url())
    urls.push(url.pathname + url.search)
    const parts = url.pathname.split('/')
    if (parts[parts.length - 1] === 'call-site') return route.fulfill({ json: callSite })
    return route.fulfill({ json: queryDetail(parts.pop() || '') })
  })
  return urls
}
