// settings.ts: Settings page payloads and routes for the dashboard-15 specs
// (the trust ceiling toggle, the pending-restart panel and source badges,
// Footprint and the audit card). Every payload names the Go test (or struct)
// whose JSON it copies. Register these routes AFTER mockAllAPIs: Playwright
// matches the newest first.

import { type Page } from '@playwright/test'
import { mockConfig } from '../fixtures'

/* ---------- Global configuration (GET/PUT /api/v1/config/global) ---------- */

// ConfigState is what the mocked server holds; GET reads it on every request.
export type ConfigState = {
  tier3Safe: string
  tier3Source: string
  generation: number
  // pending copies pendingEntry (internal/api/config_pending.go).
  pending?: unknown[]
  restartSupported?: boolean
  // masked copies config.MaskedKey (internal/config/override_mask.go).
  masked?: unknown[]
}

export function configState(over: Partial<ConfigState> = {}): ConfigState {
  return { tier3Safe: 'true', tier3Source: 'file/default', generation: 1, ...over }
}

// configBody copies configGlobalGetHandler's response: the merged config plus
// desired/active generation, pending_restart, pending and restart_supported.
export function configBody(s: ConfigState) {
  const pending = s.pending ?? []
  return {
    ...mockConfig,
    config: {
      ...mockConfig.config,
      'trust.tier3_safe': { value: s.tier3Safe, source: s.tier3Source },
      'safety.cpu_ceiling_pct': { value: 80, source: 'file/default' },
    },
    desired_generation: s.generation,
    active_generation: s.generation,
    pending_restart: pending.map(p => (p as { key: string }).key),
    pending,
    restart_supported: s.restartSupported ?? false,
    masked_by_override: s.masked ?? [],
  }
}

// applyResult copies config.ApplyResult for a live trust ceiling
// (TestGlobalPutTrustCeilingIsAppliedNotPendingRestart): applied, nothing
// pending restart.
export function applyResult(generation: number, applied: string[]) {
  return {
    desired_generation: generation, active_generation: generation,
    applied, pending_restart: [], component_status: {},
  }
}

export type PutAnswer = { status: number, json: unknown } | null

// routeConfig answers GET from state and records each PUT body. answer may
// refuse a PUT (a 400 leaves state as it was); otherwise the PUT is applied:
// tier3_safe becomes a UI override and the generation moves on.
export async function routeConfig(page: Page, state: ConfigState,
  answer: (body: Record<string, unknown>) => PutAnswer = () => null) {
  const puts: Record<string, unknown>[] = []
  await page.route('**/api/v1/config/global', route => {
    const request = route.request()
    if (request.method() !== 'PUT') {
      return route.fulfill({ json: configBody(state) })
    }
    const body = request.postDataJSON() as Record<string, unknown>
    puts.push(body)
    const refused = answer(body)
    if (refused) return route.fulfill(refused)
    if (typeof body['trust.tier3_safe'] === 'string') {
      state.tier3Safe = body['trust.tier3_safe'] as string
      state.tier3Source = 'override'
    }
    state.generation += 1
    const applied = Object.keys(body).filter(k => k !== 'expected_generation')
    return route.fulfill({ json: applyResult(state.generation, applied) })
  })
  return { puts }
}

// The two pending entries TestConfigGlobalGet_PendingCarriesActiveAndDesired
// and TestConfigGlobalGet_PendingHidesSecretValues pin: a restart-bound value
// shown as running -> after restart, and a secret listed by key only.
export const pendingEntries = [
  { key: 'collector.interval_seconds', active: 60, desired: 120, lifecycle: 'restart' },
  { key: 'llm.api_key', hidden: true, lifecycle: 'restart' },
]

// maskedCpu copies a config.MaskedKey: the file sets 90, a UI override 80.
export const maskedCpu = {
  key: 'safety.cpu_ceiling_pct', file_value: '90', override_value: '80',
  effective_value: '80', updated_by: 1, updated_at: '2026-11-13T08:00:00Z',
}

/* ---------- Footprint (GET /api/v1/self-cost) ---------- */

// footprintDatabase copies one SelfCostDatabase of
// TestSelfCostRoute_AfterTwoCyclesReportsKnownFigures; measured=false is the
// first-cycle row whose figures are all null (not 0).
export function footprintDatabase(name: string, measured: boolean) {
  const unknown = {
    database: name, known: { window: false, statements: false, storage: false },
    window_seconds: null, cycle_seconds: null, db_time_ms_per_hour: null,
    db_time_budget_ms_per_hour: null, db_time_budget_source: 'none',
    calls_per_hour: null, blocks_per_hour: null, blocks_budget_per_hour: null,
    rows_read_per_hour: null, rows_written_per_hour: null, schema_bytes: null,
    storage_budget_bytes: null, unattributed_share: null,
    top_statements: [], by_component: [],
  }
  if (!measured) return unknown
  return {
    ...unknown,
    known: { window: true, statements: true, storage: true },
    window_seconds: 600, cycle_seconds: 60, db_time_ms_per_hour: 2400,
    db_time_budget_ms_per_hour: 180000,
    db_time_budget_source: 'analyzer.self_cost_budget_ms',
    calls_per_hour: 90, blocks_per_hour: 360, blocks_budget_per_hour: 18000000,
    rows_read_per_hour: 3600, rows_written_per_hour: 360, schema_bytes: 8388608,
    storage_budget_bytes: 10737418240, unattributed_share: 0.1,
    by_component: [
      { component: 'collector', db_time_ms_per_hour: 1800, calls_per_hour: 60,
        blocks_per_hour: 300, share: 0.75 },
      { component: 'analyzer', db_time_ms_per_hour: 600, calls_per_hour: 30,
        blocks_per_hour: 60, share: 0.25 },
    ],
  }
}

export function footprintBody(databases: unknown[], cpu: number | null) {
  return {
    meaning: 'What pg_sage itself costs each database it monitors.',
    databases,
    process: { known: cpu !== null, cpu_ms_per_cycle: cpu,
      cpu_budget_ms_per_cycle: 600, top_loops: [] },
  }
}

export async function routeFootprint(page: Page,
  answer: () => { status?: number, json: unknown }) {
  await page.route(url => url.pathname === '/api/v1/self-cost', route =>
    route.fulfill(answer()))
}

/* ---------- Audit trail (GET /api/v1/audit/status) ---------- */

export const VERIFIED_AT = '2026-11-13T04:00:00Z'

// auditChain copies one chain of auditStatusHandler: ok=false with a
// row_edited problem is TestHealthRecordsRealVerification's tampered
// action_log row (triggers disabled, one row edited).
export function auditChain(ok: boolean) {
  return {
    database: 'orders', chain: 'action_log', ok, mode: 'full',
    problems: ok ? {} : { row_edited: 1 }, problems_truncated: false,
    head_seq: 9, verified_at: VERIFIED_AT,
  }
}

export function auditBody(ok: boolean) {
  return {
    ok, verified_at: VERIFIED_AT, verify_interval_hours: 24,
    chains: [auditChain(ok)], verify_errors: [],
    siem: { enabled: false, sinks: [] },
    evidence: { signed: true, key_id: 'ab12cd34', source: 'generated' },
  }
}

export async function routeAudit(page: Page, ok: () => boolean) {
  await page.route(url => url.pathname === '/api/v1/audit/status', route =>
    route.fulfill({ json: auditBody(ok()) }))
}
