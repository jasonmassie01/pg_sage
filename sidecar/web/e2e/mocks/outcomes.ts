// outcomes.ts: outcome ledger payloads (ux-02a) in the shape of
// outcomes.Result (internal/outcomes/row.go) as GET /api/v1/outcomes
// serves it (TestOutcomesRouteDB_MatchesTheSharedReadAcrossDatabases).
// Register these routes AFTER mockAllAPIs: Playwright matches the newest
// first.

import { type Page } from '@playwright/test'

export const ledgerRow = {
  action_id: 12, database: 'primary', action_type: 'create_index', what: 'Add an index',
  why: 'orders lookups read 1.2 GB per call', origin: 'approved',
  expected: { metric: 'blocks_per_call', change_pct: -40, source: 'hypopg' },
  measured: { metric: 'blocks_per_call', change_pct: -42 },
  verdict: 'improved', rollback_state: 'available', evidence_id: 'ev-xid-7',
  executed_at: '2026-11-16T03:00:00Z',
}

export const ledger = {
  since: '24h', rows: [ledgerRow], truncated: false, errors: [],
  summary: { changes: 1, verified: 1, reverted: 0, inconclusive: 0, pending: 0,
    measured: 1, measured_share: 1 },
}

// routeOutcomes serves the ledger and returns the query strings it saw.
export async function routeOutcomes(page: Page) {
  const seen: string[] = []
  await page.route(url => url.pathname === '/api/v1/outcomes', route => {
    seen.push(new URL(route.request().url()).search)
    return route.fulfill({ json: ledger })
  })
  return seen
}

// valueWithIncident is the Value payload with one credited incident whose
// evidence id the ledger row cites.
export const valueWithIncident = {
  dba_hours_saved: { all_time: 4, this_month: 4, this_week: 4 },
  by_feature: { index: 4 }, by_database: [{ name: 'primary', hours: 4 }],
  incidents_avoided: { count: 1, credited_hours: 2, detail: [{ kind: 'xid_wraparound',
    severity: 'near_miss', when: '2026-11-16T02:15:00Z', evidence_id: 'ev-xid-7' }] },
  potential_hours_pending: 0, trend_daily: [{ day: '2026-11-16', hours: 4 }],
}
