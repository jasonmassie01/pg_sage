// cases.ts: Cases page payloads and routes for the dashboard-13 specs.
// Every payload names the Go test (or struct) whose JSON it copies, so a
// renamed field on the server shows up as a failing spec here. Register
// these routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page, type Route } from '@playwright/test'

/* ---------- Fleet inventory (GET /api/v1/databases) ---------- */

// fleetDatabase copies fleet.DatabaseStatus as FleetStatus() returns it
// (fleet-06, internal/fleet budget_status_test.go); connected=false is a
// database whose pool is down.
export function fleetDatabase(name: string, id: number, connected: boolean) {
  return {
    name, id, database_id: id,
    status: {
      connected, error: connected ? null : 'connection refused',
      health_score: connected ? 95 : 0, health_reasons: connected ? [] : ['not connected'],
      trust_level: 'advisory', findings_open: connected ? 2 : 0,
      findings_critical: 0, findings_warning: connected ? 2 : 0,
      platform: 'self-managed', pg_version: '17.2',
    },
    emergency_stopped: false,
  }
}

export function fleetInventory(reportingConnected = true) {
  return {
    summary: {
      total_databases: 2, healthy: reportingConnected ? 2 : 1,
      degraded: reportingConnected ? 0 : 1, total_critical: 0, total_warning: 2,
      emergency_stopped: false,
    },
    databases: [
      fleetDatabase('orders', 1, true),
      fleetDatabase('reporting', 2, reportingConnected),
    ],
    total: 2,
  }
}

// The managed (metadata database) records of the same two databases, as
// GET /api/v1/databases/managed lists them (fleet-06 DatabasesPage).
export const managedFleet = {
  databases: [
    { id: 1, name: 'orders', host: 'orders.db', port: 5432, database_name: 'orders',
      trust_level: 'advisory', execution_mode: 'approval', enabled: true },
    { id: 2, name: 'reporting', host: 'reporting.db', port: 5432,
      database_name: 'reporting', trust_level: 'advisory', execution_mode: 'approval',
      enabled: true },
  ],
}

const isPath = (path: string) => (url: URL) => url.pathname === path

// routeFleet answers the fleet inventory; reportingConnected() is read on
// every request, so a spec can bring the database back between polls.
export async function routeFleet(page: Page, reportingConnected: () => boolean = () => true) {
  await page.route(isPath('/api/v1/databases'), route =>
    route.fulfill({ json: fleetInventory(reportingConnected()) }))
  await page.route(isPath('/api/v1/databases/managed'), route =>
    route.fulfill({ json: managedFleet }))
}

/* ---------- Cases (GET /api/v1/cases) ---------- */

// incidentCase copies the recurrence of
// TestIncidentCases_RecurrenceLinksTheResolvedPredecessor (third occurrence,
// previous resolved 2026-11-02) and the urgency of
// TestUrgency_DiskRunwayFromTheRunwayProducer (259200 s until the disk is full).
export const DISK_BASIS = 'seconds_to_limit 259200 from 48 samples over 6 h, R² 0.97'

export function incidentCase(overrides: Record<string, unknown> = {}) {
  return {
    id: 'incident:orders:deterministic:3f1c', case_id: 'incident:orders:deterministic:3f1c',
    source_type: 'incident', source_ids: ['3f1c'], database_name: 'orders',
    identity_key: 'incident:orders:deterministic:3f1c',
    title: 'Disk usage is growing toward the volume limit', severity: 'warning',
    state: 'open', why: '', why_now: `About 3 days until the disk is full (${DISK_BASIS})`,
    evidence: [], observed_at: '2026-11-12T10:00:00Z', updated_at: '2026-11-12T10:00:00Z',
    urgency: {
      assessed: true, time_to_impact_seconds: 259200, impact: 'until the disk is full',
      basis: DISK_BASIS,
    },
    recurrence: {
      previous_id: '2a0b', count: 3, previous_resolved_at: '2026-11-02T00:00:00Z',
    },
    ...overrides,
  }
}

// findingCase is a finding-backed case (cases.ProjectFinding): its
// source id is the sage.findings id an Ask Sage citation names (mcp-05).
// The category picks the card's plain-language title (casePresentation).
export function findingCase(id: string, database: string, category = 'stale_statistics',
  overrides: Record<string, unknown> = {}) {
  const key = `finding:${database}:${category}:table:public.t${id}`
  return {
    id: key, case_id: key, source_type: 'finding', source_ids: [id],
    database_name: database, identity_key: key,
    title: `${category} on public.t${id}`, severity: 'warning',
    state: 'open', why: '', why_now: '', evidence: [],
    observed_at: '2026-11-12T09:00:00Z', updated_at: '2026-11-12T09:00:00Z',
    urgency: { assessed: false },
    ...overrides,
  }
}

// queryHintCase copies cases.ProjectQueryHint (query_hint_projector.go):
// the evidence carries the verified queryid as a decimal string.
export const HINT_QUERYID = '-5191732810777595558'

export function queryHintCase(queryid: string = HINT_QUERYID) {
  const key = `query_hint:orders:${queryid}`
  return {
    id: key, case_id: key, source_type: 'query_hint', source_ids: [queryid],
    database_name: 'orders', identity_key: key,
    title: `Query hint active for query ${queryid}`, severity: 'info', state: 'open',
    why: '', why_now: '', observed_at: '2026-11-12T08:00:00Z',
    updated_at: '2026-11-12T08:00:00Z', urgency: { assessed: false },
    evidence: [{
      type: 'query_hint', summary: 'Hint Set(work_mem "64MB") is active',
      detail: { queryid, hint_text: 'Set(work_mem "64MB")', status: 'active' },
      queryid,
    }],
  }
}

// routeCases answers every /api/v1/cases request with body(url).
export async function routeCases(page: Page, body: (url: URL) => unknown) {
  await page.route('**/api/v1/cases**', (route: Route) =>
    route.fulfill({ json: body(new URL(route.request().url())) }))
}

// partialCases copies TestCasesFleetAnswersPartialWhenOneDatabaseFails:
// 200 with the readable database's cases and casesUnavailable
// {database, error_class} for the one that failed (cases_partial.go).
export function partialCases(reportingDown: boolean) {
  const cases = [findingCase('41', 'orders')]
  if (!reportingDown) cases.push(findingCase('77', 'reporting'))
  return {
    cases, total: cases.length,
    ...(reportingDown ? { unavailable: [{ database: 'reporting', error_class: 'connection' }] }
      : {}),
  }
}

/* ---------- Investigations (sre-02) ---------- */

// investigationRecord copies sre.Record as the investigations list and
// detail return it (internal/sre/record.go; TestStaleResume_* in
// internal/sre/stale_resume_db_test.go resume a paused run).
export function investigationRecord(state: string, summary: Record<string, unknown> = {}) {
  return {
    id: 'inv-7', database: 'orders', case_id: 'incident:orders:deterministic:3f1c',
    state, trigger_kind: 'disk_runway', pinned: false, version: 3, attempt: 1,
    created_at: '2026-11-12T10:00:00Z',
    summary: { family: 'disk_runway', conclusive: state === 'concluded', ...summary },
  }
}

export function investigationDetail(record: ReturnType<typeof investigationRecord>) {
  return {
    investigation: record, hypotheses: [], revisions: 1, evidence: [],
    evidence_available: true, tombstones: [], event_count: 3, chain_verified: true,
  }
}

// routeInvestigations answers the case cards' list and the panel's detail
// with current(); its sub-resources (events, proposals, similar) are empty.
export async function routeInvestigations(page: Page,
  current: () => ReturnType<typeof investigationRecord>) {
  await page.route('**/api/v1/investigations**', route =>
    route.fulfill({ json: { items: [current()] } }))
  await page.route('**/api/v1/databases/*/investigations/**', route => {
    const path = new URL(route.request().url()).pathname
    if (/\/investigations\/[^/]+$/.test(path)) {
      return route.fulfill({ json: investigationDetail(current()) })
    }
    return route.fulfill({ json: {} })
  })
}
