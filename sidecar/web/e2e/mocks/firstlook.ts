// firstlook.ts: first-look and provider-readiness payloads for
// first-look.spec.ts (firstlook-01 CHECK-03, rca-09 CHECK-30, tuning-04
// CHECK-07). Each payload names the Go code whose JSON it copies. Register
// these routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

/* ---------- First look (GET /api/v1/first-look) ---------- */

// SEQUENCE_GRANT is rolegrants.SequenceSelectSQL(["app"], "sage_agent")
// with unreadableNote's trailing semicolon.
export const SEQUENCE_GRANT = 'GRANT SELECT ON ALL SEQUENCES IN SCHEMA app TO sage_agent;'

// sequenceRunwayDegraded is the sequence_runway check unreadableNote
// (internal/firstlook/checks.go) writes when 3 sequences cannot be read.
export const sequenceRunwayDegraded = {
  rule: 'sequence_runway', status: 'degraded',
  note: '3 sequences are not readable by the pg_sage role, so their runway is unknown: ' +
    SEQUENCE_GRANT,
}

export const OK_CHECKS = [
  { rule: 'invalid_index', status: 'ok' },
  { rule: 'duplicate_index', status: 'ok' },
  { rule: 'xid_runway', status: 'ok', note: 'age(datfrozenxid) 10, mxid_age(datminmxid) 1' },
]

// LOG_RCA_DETAIL and LOG_RCA_STEPS are LogRCACapability for stderrPG17()
// (internal/firstlook/log_rca_test.go: PG 17, log_destination stderr,
// logging_collector off) on a self-managed server; LogRCAItem joins the
// steps with a space as the item's recommendation.
export const LOG_RCA_DETAIL = 'log_destination is "stderr" and logging_collector is "off": ' +
  'pg_sage reads only csvlog or jsonlog files written by the logging collector.'
export const LOG_RCA_STEPS = [
  "As a superuser: ALTER SYSTEM SET log_destination = 'stderr,jsonlog';",
  "ALTER SYSTEM SET logging_collector = on; ALTER SYSTEM SET lc_messages = 'C';",
  'Then restart PostgreSQL (logging_collector applies only at server start).',
  "Mount the server's log_directory read-only into the sidecar and set " +
    'logwatch.enabled: true and rca.enabled: true.',
]

export const logRCAItem = {
  rule: 'log_rca_unavailable', severity: 'info', object: 'log_rca',
  title: "Log-based RCA cannot read this server's log",
  detail: LOG_RCA_DETAIL, recommendation: LOG_RCA_STEPS.join(' '),
  evidence: [{ source: 'pg_settings', ref: 'log_destination, logging_collector, lc_messages',
    detail: 'log_rca status missing' }],
}

// report copies firstlook.Report's JSON tags.
export function report(database: string, extra: Record<string, unknown> = {}) {
  return {
    id: 7, database, provider: 'self-managed',
    started_at: '2026-11-10T09:00:00Z', finished_at: '2026-11-10T09:00:00.3Z',
    duration_ms: 300, relations: 1240, statement_timeout_ms: 5000,
    items: [], checks: OK_CHECKS, capabilities: [], ...extra,
  }
}

// routeFirstLook answers /api/v1/first-look with firstLookHandler's
// {"databases": [{database, report, error}]}, or answer() when it gives one.
export async function routeFirstLook(page: Page,
  answer: () => { status?: number, json: unknown }) {
  await page.route('**/api/v1/first-look**', route => {
    if (route.request().method() !== 'GET') return route.fallback()
    return route.fulfill(answer())
  })
}

/* ---------- Provider readiness (GET /api/v1/fleet/readiness) ---------- */

// HINT_LIMITATION is hintPlanLimitation (internal/fleet/runtime_capabilities.go)
// for an availability whose only reason is "enable_hint_table off".
export const HINT_LIMITATION = 'pg_hint_plan hints do not reach application sessions ' +
  '(enable_hint_table off); query hints stay advisory'

// hintPlan copies tuner.HintPlanAvailability's JSON tags.
function hintPlan(effect: boolean) {
  return {
    shared_preload: true, preload: true, hint_table_on: effect, hint_table_ready: true,
    privilege: true, application_effect: effect, session_load: false,
    method: effect ? 'shared_preload' : 'unavailable',
    ...(effect ? {} : { reasons: ['enable_hint_table off'] }),
  }
}

// readinessRow copies fleet.DatabaseReadiness: pg_hint_plan installed and
// loaded (hintModuleState "available"), its hints reaching sessions or not.
export function readinessRow(name: string, hintsTakeEffect: boolean) {
  return {
    name, provider: 'self-managed', ready_for_auto_safe: true,
    capabilities: {
      provider: 'self-managed', is_replica: false, ready_for_auto_safe: true,
      permissions: { analyze: { status: 'ok', reason: 'role is a superuser' } },
      extensions: { pg_stat_statements: 'available', pg_hint_plan: 'available' },
      ...(hintsTakeEffect ? {} : { limitations: [HINT_LIMITATION] }),
      hint_plan: hintPlan(hintsTakeEffect),
    },
  }
}

export async function routeReadiness(page: Page, rows: unknown[]) {
  await page.route('**/api/v1/fleet/readiness**', route => route.fulfill({ json: {
    mode: 'fleet',
    summary: { total_databases: rows.length, ready_for_auto_safe: rows.length, blocked: 0,
      unknown: 0 },
    databases: rows,
  } }))
}
