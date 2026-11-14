// trust.ts: Trust page and SLO payloads and routes for the dashboard-15
// specs (earned-06 holds until, earned-07 major-version cap, playbooks-07
// coverage badge). Every payload names the Go test (or struct) whose JSON it
// copies. Register these routes AFTER mockAllAPIs: Playwright matches the
// newest first.

import { type Page } from '@playwright/test'

/* ---------- Trust (GET /api/v1/trust) ---------- */

const DAY = 24 * 60 * 60 * 1000

// BENCH_GENERATED is decayGenerated in internal/earned/decay_db_test.go's
// role: the bench report the L3 pair's bench_fresh check reads. It ages out
// BenchMaxAge (30 days) later.
export const BENCH_GENERATED = '2026-11-01T09:30:00Z'
export const BENCH_EXPIRES = new Date(Date.parse(BENCH_GENERATED) + 30 * DAY).toISOString()

// incidentRow copies earned.TrustRow for wraparound_runway/freeze, the pair
// TestDecayViewsShowHoldsUntil reads. While the report is fresh the server
// sends holds_until = generated + 30 days with expiring_checks [bench_fresh]
// (Service.holding); at 30 days bench_fresh fails, supported drops to L2 and
// holds_until is left out (a level it cannot hold is not held).
export function incidentRow(reportAgeDays: number) {
  const fresh = reportAgeDays < 30
  return {
    family: 'wraparound_runway', kind: 'incident', class: 'freeze', level: 'L3',
    effective: fresh ? 'L3' : 'L2', supported: fresh ? 'L3' : 'L2', cap: 'L3',
    reversibility: 'reversible', provenance: 'ledger', evidence: {},
    last_change: { at: '2026-10-20T10:00:00Z', by: 'admin@test.com',
      event: 'promotion_approved', reason: 'bench and shadow evidence met' },
    bench: { id: 'bench-9', origin: 'ci', provenance: 'ci (signed)',
      generated_at: BENCH_GENERATED },
    ...(fresh ? { holds_until: BENCH_EXPIRES, expiring_checks: ['bench_fresh'] } : {}),
  }
}

// MAJOR_CAP_TEXT is the text TestMajorCapTrustRow pins for a self-initiated
// L3 granted on PostgreSQL 16 and read on 17 with one improved outcome.
export const MAJOR_CAP_TEXT =
  'capped at L2: PostgreSQL 16→17; 1 of 3 improved outcomes, or re-approve'

// selfRow copies the hygiene/vacuum TrustRow of TestMajorCapTrustRow:
// capped (effective L2, major_cap set) until an admin re-approves it.
export function selfRow(capped: boolean) {
  return {
    family: 'hygiene', kind: 'self_initiated', class: 'vacuum', level: 'L3',
    effective: capped ? 'L2' : 'L3', supported: 'L3', cap: 'L3',
    reversibility: 'reversible', provenance: 'ledger',
    evidence: { improved: 7, neutral: 2 }, server_major: 17,
    last_change: { at: '2026-10-01T10:00:00Z', by: 'admin@test.com',
      event: 'promotion_approved' },
    ...(capped ? { major_cap: { from: 16, to: 17, improved: 1, needed: 3,
      text: MAJOR_CAP_TEXT } } : {}),
  }
}

// TRUST_MEANING opens trustMeaning (internal/api/trust_routes.go).
export const TRUST_MEANING = "The trust ledger decides each self-initiated action class's "
  + 'level per database from verified outcomes.'

// trustBody copies the trust handler's {databases, enforced, meaning}, one
// trustDatabaseView per database (earned.TrustView plus shadow).
export function trustBody(rows: unknown[]) {
  return {
    enforced: true, meaning: TRUST_MEANING,
    databases: rows.length === 0 ? [] : [{
      database: 'orders', generated_at: '2026-11-13T09:00:00Z',
      floor: { known: false }, rows, shadow: [],
    }],
  }
}

export type TrustAnswer = { status?: number, json: unknown }

// routeTrust answers GET /api/v1/trust from answer() on every request and
// records each POST /api/v1/sre/autonomy/revalidate body.
export async function routeTrust(page: Page, answer: () => TrustAnswer,
  onRevalidate: () => void = () => {}) {
  const revalidations: unknown[] = []
  await page.route(url => url.pathname === '/api/v1/trust', route =>
    route.fulfill(answer()))
  await page.route(url => url.pathname === '/api/v1/sre/autonomy/revalidate', route => {
    revalidations.push({ url: route.request().url(), body: route.request().postDataJSON() })
    onRevalidate()
    return route.fulfill({ json: { ok: true } })
  })
  return revalidations
}

/* ---------- SLOs (GET /api/v1/sre/slos) ---------- */

const burn = (window: string, rate: number) => ({ window, burn_rate: rate })

// slo copies one slo.View of TestSLOListReportsCoverageUnverified: a
// Prometheus SLO without coverage_query is coverage_unverified, a push SLO
// is not.
export function slo(name: string, source: string, coverageUnverified: boolean) {
  return {
    name, database: 'orders', kind: 'app', source, description: `${name} availability`,
    target: 0.999, window: '30d', state: 'ok', fast_burning: false,
    customer_impact: false, unknown: [], budget_remaining: 0.8,
    evaluated_at: '2026-11-13T09:00:00Z', coverage_unverified: coverageUnverified,
    rules: [{ severity: 'page', factor: 14.4, firing: false,
      long: burn('1h', 0.4), short: burn('5m', 0.3) }],
  }
}

export async function routeSLOs(page: Page, slos: unknown[]) {
  await page.route(url => url.pathname === '/api/v1/sre/slos', route =>
    route.fulfill({ json: { slos, unavailable: [] } }))
}
