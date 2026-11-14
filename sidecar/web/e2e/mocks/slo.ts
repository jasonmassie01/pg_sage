// slo.ts: SLO list, detail and investigation payloads for slo-burn.spec.ts
// (playbooks-09 CHECK-03: a burning proxy SLO showing its duration, its
// state changes and its investigation). Each payload names the Go code
// whose JSON it copies. Register these routes AFTER mockAllAPIs:
// Playwright matches the newest first.

import { type Page } from '@playwright/test'

// NOW is the spec's browser clock; the burn started 40 minutes before it.
export const NOW = '2026-11-13T10:00:00Z'
export const ago = (minutes: number) =>
  new Date(Date.parse(NOW) - minutes * 60000).toISOString()
export const BURN_STARTED = ago(40)
export const SLO_NAME = 'pg_proxy_latency'

const burn = (window: string, rate: number) =>
  ({ window, burn_rate: rate, bad: rate * 10, eligible: 1000 })

// proxySLO copies slo.Status (internal/sre/slo/evaluate.go) for a proxy SLI
// on orders: burning is a page-level burn whose budget window is partly
// covered (budget_coverage and budget_spent_so_far, playbooks-09); not
// burning is the same SLO back at ok.
export function proxySLO(burning: boolean) {
  return {
    name: SLO_NAME, database: 'orders', kind: 'proxy', source: 'proxy',
    description: 'p95 statement latency under 250 ms', target: 0.99, window: '30d',
    state: burning ? 'page' : 'ok', fast_burning: burning, customer_impact: false,
    unknown: [], evaluated_at: ago(1), coverage_unverified: false,
    rules: [{ severity: 'page', factor: 14.4, firing: burning,
      long: burn('1h', burning ? 16.2 : 0.4), short: burn('5m', burning ? 18 : 0.2) }],
    budget_remaining: null, budget_unknown: 'partial_window', budget_coverage: 0.899,
    budget_spent_so_far: burning ? 0.12 : 0.13,
    state_since: burning ? BURN_STARTED : ago(0.5), definition_hash: 'd41d8c',
    ...(burning ? { burn_started_at: BURN_STARTED } : {}),
  }
}

// sloDetail copies sloDetailHandler's {database, status, transitions,
// recovery}, transitions newest first; recovery is sent only for a ?since=
// read (internal/api/sre_slo_handlers.go), with slo.Recovery's state,
// reason and evaluated.
export function sloDetail(withRecovery: boolean) {
  return {
    database: 'orders', status: proxySLO(true),
    transitions: [{ from: 'ticket', to: 'page', at: BURN_STARTED },
      { from: 'ok', to: 'ticket', at: ago(55) }, { to: 'ok', at: ago(60 * 24 * 3) }],
    ...(withRecovery ? { recovery: { state: 'not_recovered', reason: 'still_burning',
      since: BURN_STARTED, evaluated: 12, slices: [] } } : {}),
  }
}

// sloInvestigation copies sre.Record for the slo:<name> case the SLO's
// page-level burn opened (internal/sre/record.go).
export const sloInvestigation = {
  id: 'inv-slo-1', database: 'orders', case_id: `slo:${SLO_NAME}`, state: 'collecting',
  trigger_kind: 'slo_burn', pinned: false, version: 1, attempt: 1, created_at: ago(39),
  summary: { family: 'slo_burn', conclusive: false },
}

export type Answer = { status?: number, json: unknown }

// routeSLOBurn answers the list from list(), the detail from detail() and
// the investigation lookup from investigations(); the detail and lookup
// URLs are recorded.
export async function routeSLOBurn(page: Page, list: () => Answer, detail: () => Answer,
  investigations: () => Answer) {
  const reads: string[] = []
  await page.route(url => url.pathname === '/api/v1/sre/slos', route => route.fulfill(list()))
  await page.route(url => url.pathname === `/api/v1/sre/slos/${SLO_NAME}`, route => {
    reads.push(route.request().url())
    return route.fulfill(detail())
  })
  await page.route(url => url.pathname === '/api/v1/investigations', route => {
    reads.push(route.request().url())
    return route.fulfill(investigations())
  })
  return reads
}
