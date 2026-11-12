package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
)

// market-02 integration: the baseline store on real PostgreSQL. Three
// earlier weeks of the current hour are backfilled; then a pg_sleep
// statement runs slow. On the database whose baseline was fast the
// anomaly appears after two cycles; on a fleet database whose baseline
// for the same statement was slow, nothing fires: each database keeps its
// own baseline.
func TestWorkloadAnomaly_IntegrationSlowSleepPerDatabase(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	const orders, billing = "market02_orders", "market02_billing"
	clear := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.baseline_samples
			WHERE database IN ($1, $2)`, orders, billing)
	}
	clear()
	t.Cleanup(clear)
	st := baseline.NewStore(pool)
	w := baseline.Window{Loc: time.UTC, Weeks: 4}
	at := time.Now().UTC().Truncate(time.Hour).Add(10 * time.Minute)
	for i := 1; i <= 3; i++ {
		past := at.Add(-time.Duration(i) * 7 * 24 * time.Hour)
		for db, ms := range map[string]float64{orders: 5, billing: 400} {
			if err := st.Accumulate(ctx, db, w, past, []baseline.Sample{{Key: qkey(reportQ),
				How: "interval_mean", Value: ms}}); err != nil {
				t.Fatalf("backfill %s week -%d: %v", db, i, err)
			}
		}
	}
	const calls = 5
	start := time.Now()
	for i := 0; i < calls; i++ {
		if _, err := pool.Exec(ctx, `SELECT pg_sleep(0.04)`); err != nil {
			t.Fatalf("pg_sleep: %v", err)
		}
	}
	meanMs := float64(time.Since(start).Milliseconds()) / calls
	if meanMs < 40 {
		t.Fatalf("pg_sleep(0.04) mean %.1f ms, want at least 40", meanMs)
	}
	run := func(db string) []Finding {
		a := anomalyAnalyzer(t, st)
		a.WithDatabaseName(db)
		var got []Finding
		for i := 0; i < 2; i++ {
			cur, prev := snapPair(at.Add(time.Duration(i)*time.Minute),
				qspec{reportQ, meanMs, calls})
			got = runAnomalyCycle(a, cur, prev)
			if a.eval.hasFailed(CategoryWorkloadAnomaly) {
				t.Fatalf("%s cycle %d: workload_anomaly failed", db, i)
			}
		}
		return got
	}
	got := run(orders)
	if len(got) != 1 || got[0].ObjectIdentifier != "query_mean_ms:queryid:101" ||
		got[0].Detail["weeks"] != 3 || got[0].Detail["baseline_median"] != 5.0 {
		t.Fatalf("orders = %+v, want one anomaly against the 5 ms baseline", got)
	}
	if got := run(billing); len(got) != 0 {
		t.Fatalf("billing = %+v, want none: its baseline is 400 ms", got)
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT n FROM sage.baseline_samples
		WHERE database = $1 AND metric = 'query_mean_ms' AND key = 'queryid:101'
		  AND week_start = $2::date`, orders, w.WeekStart(at)).Scan(&n); err != nil || n != 2 {
		t.Fatalf("this week's bucket n = %d (%v), want the 2 cycles accumulated", n, err)
	}
}
