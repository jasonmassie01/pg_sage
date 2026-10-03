package runway

import (
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
