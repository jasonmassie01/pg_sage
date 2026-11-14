// fleet.ts: fleet inventory, onboarding and first-look payloads for
// fleet-health.spec.ts (fleet-07: the health-score tooltip and the fleet
// order; firstlook-03 CHECK-05: a two-database fleet with one database
// refusing connections). Each payload names the Go code whose JSON it
// copies. Register these routes AFTER mockAllAPIs: Playwright matches the
// newest first.

import { type Page } from '@playwright/test'
import { logRCAItem, report } from './firstlook'

/* ---------- Fleet inventory (GET /api/v1/databases) ---------- */

// scored copies fleet.DatabaseStatus with healthOf's score and reasons
// (internal/fleet/health.go): "disconnected: <err>", "collector stale 42m",
// "1 critical finding", "N warnings".
export function scored(name: string, score: number, reasons: string[],
  status: Record<string, unknown> = {}) {
  return {
    name, status: {
      connected: true, error: null, health_score: score, health_reasons: reasons,
      trust_level: 'advisory', findings_critical: 0, findings_warning: 0,
      platform: 'self-managed', ...status,
    },
  }
}

export function inventory(databases: ReturnType<typeof scored>[]) {
  const healthy = databases.filter(d => d.status.connected && d.status.health_score >= 50)
  return {
    summary: {
      total_databases: databases.length, healthy: healthy.length,
      degraded: databases.length - healthy.length, total_critical: 0, total_warning: 0,
      emergency_stopped: false,
    },
    databases, total: databases.length,
  }
}

// WORST_FIRST is FleetStatus's order for TestFleetStatus_WorstFirstThenName
// plus TestFleetStatus_EqualScoresSortByName's tie: score ascending, then
// name. Neither name order nor best-first gives this order.
export const WORST_FIRST = [
  scored('mike', 0, ['disconnected: connection refused'],
    { connected: false, error: 'connection refused' }),
  scored('yankee', 75, ['1 critical finding'], { findings_critical: 1 }),
  scored('alpha', 100, []),
  scored('bravo', 100, []),
  scored('zeta', 100, []),
]

export async function routeInventory(page: Page, body: () => unknown) {
  await page.route(url => url.pathname === '/api/v1/databases', route =>
    route.fulfill({ json: body() }))
}

/* ---------- Onboarding and first look (firstlook-03) ---------- */

// step copies onboarding.Step; CONNECTED_DOWN is connectedStep's detail
// for a database the read could not reach (internal/onboarding/checklist.go).
const step = (id: string, label: string, done: boolean, detail: string) =>
  ({ id, label, done, detail })
export const CONNECTED_DOWN =
  'pg_sage cannot reach the database; check the connection URL and network.'

// onboardingEntry copies one onboardingHandler entry: the live database gets
// its checklist, the unreachable one error "unreachable" and connected:false
// (TestFleetOnboardingLiveAndDownDatabase, internal/api).
export function onboardingEntry(database: string, reachable: boolean) {
  return {
    database, install_kind: 'new', trust_level: 'observation', connected: reachable,
    installed_at: '2026-11-12T09:00:00Z',
    ...(reachable ? { time_to_first_finding_seconds: 41.2 } : { error: 'unreachable' }),
    steps: [
      step('connected', 'Connected', reachable,
        reachable ? 'pg_sage is connected and monitoring.' : CONNECTED_DOWN),
      step('extensions', 'Extensions', reachable, 'pg_stat_statements is ready.'),
      step('first_look', 'First look ready', reachable, reachable
        ? '1 finding in 1,240 relations.' : 'Reading the catalog (no query history needed).'),
      step('grant_more', 'Grant more', false, 'pg_sage is read-only.'),
    ],
  }
}

// firstLookFleet copies firstLookHandler's per-database answer: a report
// for orders, and the reporting entry with error "unreachable", no report.
export const firstLookFleet = { databases: [
  { database: 'orders', report: report('orders', { items: [logRCAItem] }) },
  { database: 'reporting', report: null, error: 'unreachable' },
] }

// routeSetup answers /api/v1/onboarding and /api/v1/first-look for the
// fleet (no database or "all") with fleet(), and for one database with
// single(name): firstlook-03's 503 for the database that refuses connections.
export async function routeSetup(page: Page, fleet: { onboarding: unknown, firstLook: unknown },
  single: (path: string, database: string) => { status?: number, json: unknown }) {
  for (const path of ['/api/v1/onboarding', '/api/v1/first-look']) {
    await page.route(url => url.pathname === path, route => {
      const database = new URL(route.request().url()).searchParams.get('database')
      if (database && database !== 'all') return route.fulfill(single(path, database))
      const json = path === '/api/v1/onboarding' ? fleet.onboarding : fleet.firstLook
      return route.fulfill({ json })
    })
  }
}
