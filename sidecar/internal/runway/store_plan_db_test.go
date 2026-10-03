package runway

import (
	"context"
	"strings"
	"testing"
)

// Performance gate: the runway sample prune and the restart load bound
// sampled_at with a stable cutoff, so the sampled_at index can serve them.
// A volatile clock_timestamp() cutoff cannot be an index bound: every pass
// read all of sage.runway_samples (reviews/2026-10-03-perf-gate-report.md).
func TestRunwayStoreStatementsAreIndexServable(t *testing.T) {
	pool, ctx := livePool(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{"prune": pruneSamplesSQL,
		"load": loadLastSQL} {
		if strings.Contains(sql, "clock_timestamp()") {
			t.Errorf("%s bounds sampled_at with volatile clock_timestamp()", name)
		}
		rows, err := conn.Query(ctx, "EXPLAIN "+sql, 172800.0, 1000)
		if name == "load" {
			rows, err = conn.Query(ctx, "EXPLAIN "+sql, 172800.0)
		}
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if strings.Contains(plan.String(), "Seq Scan on runway_samples") {
			t.Errorf("%s scans runway_samples sequentially:\n%s", name, plan.String())
		}
	}
}

// Perf gate (runway_samples, one seq scan of 20,000 rows per startup): with
// real statistics, DELETE ... WHERE id IN (subquery) may be planned as a
// hash join over a full scan of runway_samples, whatever the subquery's
// index. The batch's ids are collected into an array first.
func TestRunwayPruneDeletesThroughTheKey(t *testing.T) {
	pool, ctx := livePool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples (kind, subject, epoch,
		sampled_at, value, counter, limit_value)
		SELECT 'disk', 'plan_test_' || g, 'e', now() - g * interval '10 seconds', g, g, 1e9
		FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.runway_samples WHERE subject LIKE 'plan_test_%'`)
	})
	if _, err := pool.Exec(ctx, "ANALYZE sage.runway_samples"); err != nil {
		t.Fatal(err)
	}
	var plan string
	if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+strings.NewReplacer(
		"$1", "86400", "$2", "10000").Replace(pruneSamplesSQL)).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan, `"Seq Scan"`) {
		t.Fatalf("prune scans runway_samples sequentially:\n%s", plan)
	}
}
