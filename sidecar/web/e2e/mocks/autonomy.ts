// autonomy.ts: Earned autonomy and header payloads for the dashboard-14
// specs (fleet canaries and the emergency stop badge). Every payload names
// the Go test (or struct) whose JSON it copies. Register these routes AFTER
// mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'
import { fleetDatabase } from './cases'

/* ---------- Earned autonomy (GET /api/v1/sre/autonomy) ---------- */

// autonomyView is the smallest view autonomyHandlers.view serves for one
// database (the shape AutonomyPage.test.jsx pins): enforced, no families.
export const autonomyView = {
  database: 'orders', enforced: true, databases: ['orders'],
  view: { generated_at: '2026-11-13T09:00:00Z', game_days: [], families: [] },
}

/* ---------- Fleet canaries (GET /api/v1/sre/autonomy/rollouts) ---------- */

export const CANARY_RUN_ID = 'rollout-7'

// canaryRun copies rollout.RunRecord as CanaryService.List returns it:
// state "canary" while the canary waits on its target's verdict and
// "complete" after it (TestFleetCanaryWaitsForTheTargetVerdictOnRealDatabases).
export function canaryRun(state: string, applied: number) {
  return {
    id: CANARY_RUN_ID, source_database: 'orders', prior_evidence_id: 'ev-41',
    policy: { CanaryInstances: 1, MaxAffectedInstances: 2 }, state,
    applied_instances: applied, canary_databases: ['reporting'],
    aggregate_regression_pct: 0, created_at: '2026-11-13T08:00:00Z',
    updated_at: '2026-11-13T08:05:00Z', family: 'index', class: 'create_index',
    started_by: 'test@test.com', targets: ['reporting', 'billing'],
  }
}

// instanceRecord copies rollout.InstanceRecord (json "database", "status").
export function instanceRecord(database: string, ordinal: number, status: string) {
  return {
    database, ordinal, status, regression_pct: 0, updated_at: '2026-11-13T08:05:00Z',
  }
}

export type CanaryState = {
  runs: () => unknown[]
  // detail answers GET /rollouts/{id}: autonomyHandlers.rollout's {run, instances}.
  detail?: (id: string) => { status: number, json: unknown }
}

// routeAutonomy answers the autonomy view and the canary list and detail;
// detailAsked records each detail path, so a spec can see what was polled.
export async function routeAutonomy(page: Page, state: CanaryState) {
  const detailAsked: string[] = []
  await page.route(url => url.pathname.startsWith('/api/v1/sre/autonomy'), route => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (path === '/api/v1/sre/autonomy/rollouts') {
      return route.fulfill({ json: { enabled: true, items: state.runs() } })
    }
    if (path.startsWith('/api/v1/sre/autonomy/rollouts/')) {
      detailAsked.push(path)
      const id = decodeURIComponent(path.split('/').pop() || '')
      if (!state.detail) return route.fulfill({ status: 404, json: { error: 'not found' } })
      return route.fulfill(state.detail(id))
    }
    if (path === '/api/v1/sre/autonomy') return route.fulfill({ json: autonomyView })
    return route.fulfill({ json: { items: [] } })
  })
  return detailAsked
}

/* ---------- Emergency stop (GET /api/v1/databases summary) ---------- */

export const STOPPED_AT = '2026-11-13T07:30:00Z'

export type StopState = {
  fleetStopped: boolean
  stoppedBy?: string
  // databaseStoppedBy stops only "orders", with no fleet latch.
  databaseStoppedBy?: string
}

// stopInventory copies the summary TestFleetStopOverviewCountsLaterDatabase
// reads (emergency_stopped, emergency_stopped_count, fleet_stopped,
// fleet_stopped_by, fleet_stopped_at) over two fleet.DatabaseStatus rows.
export function stopInventory(s: StopState) {
  const orders = fleetDatabase('orders', 1, true)
  const reporting = fleetDatabase('reporting', 2, true)
  const stop = (db: ReturnType<typeof fleetDatabase>, by: string) => ({
    ...db, emergency_stopped: true, emergency_stopped_by: by,
    emergency_stopped_at: STOPPED_AT,
  })
  const by = s.stoppedBy ?? ''
  const databases = s.fleetStopped ? [stop(orders, by), stop(reporting, by)]
    : s.databaseStoppedBy ? [stop(orders, s.databaseStoppedBy), reporting]
      : [orders, reporting]
  const count = databases.filter(d => d.emergency_stopped).length
  return {
    summary: {
      total_databases: 2, healthy: 2, degraded: 0, total_critical: 0, total_warning: 4,
      emergency_stopped: count > 0, emergency_stopped_count: count,
      fleet_stopped: s.fleetStopped,
      ...(s.fleetStopped ? { fleet_stopped_by: by, fleet_stopped_at: STOPPED_AT } : {}),
    },
    databases, total: 2,
  }
}

// routeStop answers GET /api/v1/databases from state() on every request, so
// a spec can press or release the stop between fleet polls.
export async function routeStop(page: Page, state: () => StopState) {
  await page.route(url => url.pathname === '/api/v1/databases', route =>
    route.fulfill({ json: stopInventory(state()) }))
}
