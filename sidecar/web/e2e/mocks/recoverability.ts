// Restore drill payloads (drill-01d), copied from the Go routes' golden
// responses: GET /api/v1/databases/{db}/restore-drills as
// TestRestoreDrillRoutesDB_HistoryMatchesTheGolden answers it
// (internal/api/testdata/restore_drills_golden.json) and GET
// /api/v1/fleet/recoverability as TestRestoreDrillRoutesDB_FleetCountMatchesTheGolden
// answers it (internal/api/testdata/fleet_recoverability_golden.json, its
// databases list shortened to the counts the overview reads).

const invariants = [
  { name: 'catalog', table: 'app.orders', status: 'pass', evidence: 'same 4 columns' },
  { name: 'marker', status: 'pass',
    evidence: 'sage.snapshots 4m0s before the recovery point' },
]

const passed = {
  id: 2, database: 'orders', provider: 'rds_restore', kind: 'full_copy',
  requested_at: '2026-11-15T11:49:00Z', ready_at: '2026-11-15T11:59:00Z',
  finished_at: '2026-11-15T12:00:00Z', recovery_point: '2026-11-15T11:45:00Z',
  rto_seconds: 660, rpo_seconds: 240, invariants, passed: true,
  cost_label: 'about $1.40 (estimate)',
}

const failed = {
  id: 1, database: 'orders', provider: 'rds_restore', kind: 'full_copy',
  requested_at: '2026-11-07T11:51:00Z', finished_at: '2026-11-07T12:00:00Z',
  invariants: [{ name: 'row_count', table: 'app.orders', status: 'fail',
    evidence: '90 rows, source 100' }],
  passed: false, failure: 'row_count app.orders: 90 rows, source 100',
  cost_label: 'about $1.40 (estimate)',
}

// restoreDrills is orders' history; over replaces top-level fields (a
// database renames it in every row).
export function restoreDrills(over: Record<string, unknown> = {}) {
  const db = typeof over.database === 'string' ? over.database : 'orders'
  const rename = <T extends { database: string }>(r: T): T => ({ ...r, database: db })
  return {
    database: db, status: 'passed', latest: rename(passed), last_pass: rename(passed),
    window_days: 35, drills: [rename(passed), rename(failed)], isolation: null,
    next_full_copy_cost_label: 'about $1.40 (estimate)', ...over,
  }
}

export function fleetRecoverability(over: Record<string, unknown> = {}) {
  return {
    window_days: 35, total: 6, passed: 2, failed: 1, stale: 1, never_drilled: 1,
    unknown: 1, median_rto_seconds: 780, databases: [], ...over,
  }
}
