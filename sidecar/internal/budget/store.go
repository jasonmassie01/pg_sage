package budget

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Store is the shadow ledger and per-role evidence in the sage schema
// (sage.budget_shadow, sage.workload_role_daily) of one database.
type Store struct {
	pool     *pgxpool.Pool
	database string
}

// NewStore keys the ledger by database (the fleet's database name).
func NewStore(pool *pgxpool.Pool, database string) *Store {
	return &Store{pool: pool, database: database}
}

// Record adds one cycle's tallies to the day's rows: would-cancels add
// up (each statement is counted once by the evaluator), would-rejects
// and the worst statement keep the day's largest. A budget's row is
// written even with nothing to count: first_seen starts its promotion
// window when it is first measured.
func (s *Store) Record(ctx context.Context, day time.Time, tallies []Tally) error {
	batch := &pgx.Batch{}
	for _, t := range tallies {
		batch.Queue(`/* pg_sage budget */
INSERT INTO sage.budget_shadow (database_name, budget, definition, day, would_cancel,
    would_reject, protected_cancels, worst_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (database_name, budget, definition, day) DO UPDATE SET
    would_cancel = sage.budget_shadow.would_cancel + EXCLUDED.would_cancel,
    would_reject = GREATEST(sage.budget_shadow.would_reject, EXCLUDED.would_reject),
    protected_cancels = sage.budget_shadow.protected_cancels + EXCLUDED.protected_cancels,
    worst_ms = GREATEST(sage.budget_shadow.worst_ms, EXCLUDED.worst_ms),
    updated_at = now()`, s.database, t.Budget, t.Definition, day, t.WouldCancel,
			t.WouldReject, t.ProtectedCancels, t.WorstMs)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// RecordRoles keeps each measured role's largest p99 estimate and
// longest statement of the day.
func (s *Store) RecordRoles(ctx context.Context, day time.Time,
	roles []collector.WorkloadRoleStats) error {
	batch := &pgx.Batch{}
	for _, r := range roles {
		if r.Calls <= 0 {
			continue
		}
		batch.Queue(`/* pg_sage budget */
INSERT INTO sage.workload_role_daily (database_name, role_name, day, p99_estimate_ms,
    max_exec_ms, calls)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (database_name, role_name, day) DO UPDATE SET
    p99_estimate_ms = GREATEST(sage.workload_role_daily.p99_estimate_ms, EXCLUDED.p99_estimate_ms),
    max_exec_ms = GREATEST(sage.workload_role_daily.max_exec_ms, EXCLUDED.max_exec_ms),
    calls = GREATEST(sage.workload_role_daily.calls, EXCLUDED.calls),
    updated_at = now()`, s.database, r.Role, day, r.P99EstimateMs, r.MaxExecMs, r.Calls)
	}
	if batch.Len() == 0 {
		return nil
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// History is a budget definition's ledger rows since since, oldest first.
func (s *Store) History(ctx context.Context, budget, definition string,
	since time.Time) ([]ShadowDay, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage budget */
SELECT day, first_seen, would_cancel, would_reject, protected_cancels, worst_ms,
       actual_cancels, cancel_source, cancels_lower_bound
  FROM sage.budget_shadow
 WHERE database_name = $1 AND budget = $2 AND definition = $3 AND day >= $4::date
 ORDER BY day`, s.database, budget, definition, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("query sage.budget_shadow: %w", err)
	}
	defer rows.Close()
	var out []ShadowDay
	for rows.Next() {
		var d ShadowDay
		var cancels, protected int64
		if err := rows.Scan(&d.Day, &d.FirstSeen, &cancels, &d.WouldReject, &protected,
			&d.WorstMs, &d.ActualCancels, &d.CancelSource, &d.CancelsLowerBound); err != nil {
			return nil, fmt.Errorf("scan sage.budget_shadow: %w", err)
		}
		d.WouldCancel, d.ProtectedCancels = int(cancels), int(protected)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RoleEvidence is each role's largest daily p99 estimate and longest
// statement since since.
func (s *Store) RoleEvidence(ctx context.Context, since time.Time) (map[string]RoleEvidence,
	error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage budget */
SELECT role_name, max(p99_estimate_ms), max(max_exec_ms), max(calls)
  FROM sage.workload_role_daily
 WHERE database_name = $1 AND day >= $2::date
 GROUP BY role_name`, s.database, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("query sage.workload_role_daily: %w", err)
	}
	defer rows.Close()
	out := map[string]RoleEvidence{}
	for rows.Next() {
		var role string
		var ev RoleEvidence
		if err := rows.Scan(&role, &ev.P99Ms, &ev.MaxMs, &ev.Calls); err != nil {
			return nil, fmt.Errorf("scan sage.workload_role_daily: %w", err)
		}
		out[role] = ev
	}
	return out, rows.Err()
}
