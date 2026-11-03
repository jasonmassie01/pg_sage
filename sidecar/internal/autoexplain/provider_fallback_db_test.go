package autoexplain

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/schema"
)

// Provider history is scoped to a resource and policy. It cannot stand in for
// an eligible legacy plan merely because the PostgreSQL query ID matches.
func TestCandidateSQLProviderHistoryCannotSuppressFallback(t *testing.T) {
	pool := requireAuditFixturePool(t)
	ctx := t.Context()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Exercise the actual pg_stat_statements view; a temporary relation gives
	// this statement a distinct query identity without resetting shared stats.
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE fallback_candidate_fixture
		ON COMMIT DROP AS SELECT generate_series(1, 100) AS value`); err != nil {
		t.Fatal(err)
	}
	const query = "SELECT sum(value) FROM pg_temp.fallback_candidate_fixture"
	for range 12 {
		var sum int64
		if err := tx.QueryRow(ctx, query).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		if sum != 5050 {
			t.Fatalf("unexpected fixture sum: %d", sum)
		}
	}
	var queryID, calls int64
	var meanExecMS float64
	if err := tx.QueryRow(ctx, `SELECT queryid, calls, mean_exec_time
		FROM pg_stat_statements
		WHERE query = $1 AND dbid = (SELECT oid FROM pg_database
			WHERE datname = current_database())`, query).
		Scan(&queryID, &calls, &meanExecMS); err != nil {
		t.Fatalf("read actual workload statistics: %v", err)
	}
	const thresholdMS = 0.0
	if calls <= 10 || meanExecMS <= thresholdMS {
		t.Fatalf("fixture is not eligible: calls=%d mean_exec_time=%g", calls, meanExecMS)
	}
	assertCandidate := func(want bool) {
		t.Helper()
		rows, err := tx.Query(ctx, candidateSQL, thresholdMS, 100000)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := false
		for rows.Next() {
			var id int64
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				t.Fatal(err)
			}
			found = found || id == queryID
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if found != want {
			t.Fatalf("fallback candidate present=%v, want %v", found, want)
		}
	}
	assertCandidate(true)
	if _, err := tx.Exec(ctx, `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, captured_at)
		VALUES ($1, $2, '[{"Plan":{"Node Type":"Result","Total Cost":1}}]'::jsonb,
		$3, now())`, queryID, query, LakebasePlanSource); err != nil {
		t.Fatal(err)
	}
	assertCandidate(true)
	if _, err := tx.Exec(ctx, `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, captured_at)
		VALUES ($1, $2, '[{"Plan":{"Node Type":"Result","Total Cost":1}}]'::jsonb,
		'generic_plan', now())`, queryID, query); err != nil {
		t.Fatal(err)
	}
	assertCandidate(false)
}
