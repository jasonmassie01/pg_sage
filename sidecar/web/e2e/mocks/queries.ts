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

// routeQueryDetail answers the query page for any queryid and records the
// URLs asked for.
export async function routeQueryDetail(page: Page) {
  const urls: string[] = []
  await page.route('**/api/v1/queries/**', route => {
    const url = new URL(route.request().url())
    urls.push(url.pathname + url.search)
    const id = url.pathname.split('/').pop() || ''
    return route.fulfill({ json: queryDetail(id) })
  })
  return urls
}

// queriesCoverage copies GET /api/v1/queries/coverage as
// TestQueriesCoverageDB_MatchesTheGolden answers it
// (internal/api/testdata/queries_coverage_golden.json): 28 days of hourly
// coverage, 87% of the execution time covered, three hours with breaks.
// over replaces fields of the one database's entry.
export function queriesCoverage(over: Record<string, unknown> = {}) {
  return {
    database: 'testdb', since: '2021-05-01T00:00:00Z', until: '2021-05-29T00:00:00Z',
    databases: [{
      database: 'testdb', status: 'ok', covered_exec_share: 0.87, epoch_breaks: 3,
      hours: 667, hours_missing: 5, dealloc_growth: 667,
      epoch_source_counters_only_hours: 2, oldest_hour: '2021-05-01T00:00:00Z',
      oldest_day: '2021-05-01', rollup_bytes: 229376, ...over,
    }],
  }
}

// routeQueriesCoverage answers the coverage notice with respond() and
// records the URLs asked for.
export async function routeQueriesCoverage(page: Page, respond: () => unknown) {
  const urls: string[] = []
  await page.route('**/api/v1/queries/coverage**', route => {
    const url = new URL(route.request().url())
    urls.push(url.pathname + url.search)
    return route.fulfill({ json: respond() })
  })
  return urls
}
