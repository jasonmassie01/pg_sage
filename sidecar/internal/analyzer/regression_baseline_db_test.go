package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-05: the regression baseline is the median of the hourly interval
// means a query's sage.query_store samples give over the lookback,
// excluding the last hour (QueryIntervalsMany, collection-12's reader).

// No concurrent access tests: regressionBaselines is called once per cycle
// by the single analyzer goroutine and keeps no state between calls.

func clearQueryStore(t *testing.T, pool *pgxpool.Pool, lo, hi int64) {
	t.Helper()
	const sql = `DELETE FROM sage.query_store WHERE queryid BETWEEN $1 AND $2`
	if _, err := pool.Exec(context.Background(), sql, lo, hi); err != nil {
		t.Fatalf("clear query_store: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), sql, lo, hi) })
}

// seedHourly writes one query_store sample per hour for hours hours ending
// at end, perHour calls at meanMs each, in statistics epoch epoch. It
// returns the last sample's counters.
func seedHourly(t *testing.T, pool *pgxpool.Pool, qid int64, end time.Time, hours int,
	perHour int64, meanMs float64, epoch time.Time) (int64, float64) {
	t.Helper()
	var calls int64
	var total float64
	for h := hours; h >= 0; h-- {
		at := end.Add(-time.Duration(h) * time.Hour)
		if _, err := pool.Exec(context.Background(), `INSERT INTO sage.query_store
			(queryid, captured_at, calls, total_exec_time, mean_exec_time, rows, stats_epoch)
			VALUES ($1, $2, $3, $4, $5, 0, $6)`, qid, at, calls, total, meanMs,
			epoch); err != nil {
			t.Fatalf("seed query_store: %v", err)
		}
		calls += perHour
		total += float64(perHour) * meanMs
	}
	return calls - perHour, total - float64(perHour)*meanMs
}

// Integration: a week of hourly samples at 2 ms gives a 2 ms baseline even
// though the last 40 minutes ran at 20 ms (the last hour is excluded); a
// query with 5 hours of samples has no baseline; an unknown query none.
func TestRegressionBaselines_ReadHourlyMedianFromQueryStore(t *testing.T) {
	pool := phase2Pool(t)
	const steady, short, unknown = int64(8_105_000_001), int64(8_105_000_002),
		int64(8_105_000_003)
	clearQueryStore(t, pool, steady, unknown)
	now := time.Now().UTC().Truncate(time.Second)
	epoch := now.Add(-30 * 24 * time.Hour)
	calls, total := seedHourly(t, pool, steady, now.Add(-70*time.Minute), 7*24-2, 600, 2,
		epoch)
	for i := int64(1); i <= 4; i++ {
		at := now.Add(-time.Duration(50-10*i) * time.Minute)
		if _, err := pool.Exec(context.Background(), `INSERT INTO sage.query_store
			(queryid, captured_at, calls, total_exec_time, mean_exec_time, rows, stats_epoch)
			VALUES ($1, $2, $3, $4, 20, 0, $5)`, steady, at, calls+100*i,
			total+2000*float64(i), epoch); err != nil {
			t.Fatalf("seed regressed samples: %v", err)
		}
	}
	seedHourly(t, pool, short, now.Add(-2*time.Hour), 5, 600, 2, epoch)
	a := New(pool, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	got, err := a.regressionBaselines(context.Background(),
		[]int64{steady, short, unknown}, now)
	if err != nil {
		t.Fatalf("regressionBaselines: %v", err)
	}
	b, ok := got[steady]
	if !ok || !near(b.MeanMs, 2, 1e-9) || b.Buckets < 150 {
		t.Fatalf("steady baseline = %+v, %v; want 2 ms over at least 150 buckets", b, ok)
	}
	if b, ok := got[short]; ok {
		t.Fatalf("5 hourly buckets gave a baseline %+v", b)
	}
	if b, ok := got[unknown]; ok {
		t.Fatalf("an unknown query got a baseline %+v", b)
	}
}

// A zero or negative lookback reads the default 7 days, not nothing.
func TestRegressionBaselines_LookbackDefault(t *testing.T) {
	pool := phase2Pool(t)
	const qid = int64(8_105_000_011)
	clearQueryStore(t, pool, qid, qid)
	now := time.Now().UTC().Truncate(time.Second)
	seedHourly(t, pool, qid, now.Add(-3*24*time.Hour), 24, 600, 4, now.Add(-40*24*time.Hour))
	for _, days := range []int{0, -3} {
		cfg := intervalConfig()
		cfg.Analyzer.RegressionLookbackDays = days
		a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
		got, err := a.regressionBaselines(context.Background(), []int64{qid}, now)
		if err != nil || !near(got[qid].MeanMs, 4, 1e-9) {
			t.Fatalf("lookback %d: baselines %+v, %v; want 4 ms from the 7-day default",
				days, got, err)
		}
	}
}

// Boundary: the last hour is not baseline. Five hourly buckets older than
// an hour and a sixth inside the last hour give no baseline (5 < 6).
func TestRegressionBaselines_ExcludesTheLastHour(t *testing.T) {
	pool := phase2Pool(t)
	const qid = int64(8_105_000_021)
	clearQueryStore(t, pool, qid, qid)
	now := time.Now().UTC().Truncate(time.Second)
	epoch := now.Add(-30 * 24 * time.Hour)
	calls, total := seedHourly(t, pool, qid, now.Add(-70*time.Minute), 5, 600, 2, epoch)
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.query_store
		(queryid, captured_at, calls, total_exec_time, mean_exec_time, rows, stats_epoch)
		VALUES ($1, $2, $3, $4, 2, 0, $5)`, qid, now.Add(-10*time.Minute), calls+600,
		total+1200, epoch); err != nil {
		t.Fatalf("seed last-hour sample: %v", err)
	}
	a := New(pool, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	got, err := a.regressionBaselines(context.Background(), []int64{qid}, now)
	if err != nil {
		t.Fatalf("regressionBaselines: %v", err)
	}
	if b, ok := got[qid]; ok {
		t.Fatalf("a bucket of the last hour counted: baseline %+v", b)
	}
	got, err = a.regressionBaselines(context.Background(), []int64{qid},
		now.Add(time.Hour))
	if err != nil || got[qid].Buckets != 6 || !near(got[qid].MeanMs, 2, 1e-9) {
		t.Fatalf("an hour later: %+v, %v; want 2 ms over 6 buckets", got, err)
	}
}

// statementCounters reads one statement's pg_stat_statements counters in
// this database.
func statementCounters(t *testing.T, conn *pgxpool.Conn, marker string) collector.QueryStats {
	t.Helper()
	var q collector.QueryStats
	err := conn.QueryRow(context.Background(), `SELECT queryid, query, calls::bigint,
		total_exec_time, mean_exec_time FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query LIKE '%' || $1 || '%' AND query NOT LIKE '%pg_stat_statements%'
		ORDER BY calls DESC LIMIT 1`, marker).Scan(&q.QueryID, &q.Query, &q.Calls,
		&q.TotalExecTime, &q.MeanExecTime)
	if err != nil {
		t.Fatalf("read pg_stat_statements for %s: %v", marker, err)
	}
	return q
}

// runLookups runs the point lookup n times as an unnamed statement, planned
// at every execution: a cached generic plan would ignore the planner
// settings the test changes.
func runLookups(t *testing.T, conn *pgxpool.Conn, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var pad string
		if err := conn.QueryRow(context.Background(),
			"SELECT pad FROM rules05_orders WHERE id = $1", pgx.QueryExecModeExec,
			1+i*37%190_000).Scan(&pad); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
}

func execAll(t *testing.T, conn *pgxpool.Conn, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// lookupTable creates the 200,000-row lookup table and returns a dedicated
// connection for the workload.
func lookupTable(t *testing.T, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release)
	execAll(t, conn, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements",
		"DROP TABLE IF EXISTS rules05_orders",
		"CREATE TABLE rules05_orders (id int PRIMARY KEY, pad text NOT NULL)",
		"INSERT INTO rules05_orders SELECT g, repeat('x', 200) "+
			"FROM generate_series(1, 200000) g",
		"ANALYZE rules05_orders")
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS rules05_orders") })
	return conn
}

// Integration on real pg_stat_statements: a point lookup runs by index for
// a seeded week of query_store history, then its plan is forced to a
// sequential scan (enable_indexscan and enable_bitmapscan off in the
// session). The cumulative mean barely moves; the interval mean against
// the hourly median baseline is a query_regression finding.
func TestRegression_ForcedPlanChangeOnRealStatements(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	conn := lookupTable(t, pool)
	runLookups(t, conn, 3000)
	before := statementCounters(t, conn, "rules05_orders WHERE id")
	clearQueryStore(t, pool, before.QueryID, before.QueryID)
	now := time.Now().UTC().Truncate(time.Second)
	epoch := now.Add(-30 * 24 * time.Hour)
	fast := before.TotalExecTime / float64(before.Calls)
	seedHourly(t, pool, before.QueryID, now.Add(-2*time.Hour), 7*24-3, 600, fast, epoch)

	execAll(t, conn, "SET enable_indexscan = off", "SET enable_bitmapscan = off")
	runLookups(t, conn, 20)
	execAll(t, conn, "RESET enable_indexscan", "RESET enable_bitmapscan")
	after := statementCounters(t, conn, "rules05_orders WHERE id")
	if after.QueryID != before.QueryID || after.Calls != before.Calls+20 {
		t.Fatalf("statement entry changed under the test (reset elsewhere?): %+v -> %+v",
			before, after)
	}
	prev := &collector.Snapshot{CollectedAt: now.Add(-time.Minute), StatsEpoch: epoch,
		Queries: []collector.QueryStats{before}}
	cur := &collector.Snapshot{CollectedAt: now, StatsEpoch: epoch,
		Queries: []collector.QueryStats{after}}
	a := New(pool, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	got := a.runQueryRegression(ctx, cur, prev)
	want := fmt.Sprintf("queryid:%d", before.QueryID)
	if len(got) != 1 || got[0].ObjectIdentifier != want {
		t.Fatalf("findings = %+v, want one query_regression for %s (fast %.4f ms, "+
			"cumulative %.4f -> %.4f ms)", got, want, fast, before.MeanExecTime,
			after.MeanExecTime)
	}
	f := got[0]
	if a.eval.hasFailed("query_regression") || f.Detail["baseline_method"] !=
		"median_hourly_interval" || !near(detailFloat(t, f, "baseline_ms"), fast, 1e-6) ||
		detailFloat(t, f, "interval_mean_ms") < 5*fast {
		t.Fatalf("finding detail = %v (fast %.4f ms)", f.Detail, fast)
	}
	t.Logf("fast %.4f ms, cumulative %.4f ms, interval %.4f ms", fast, after.MeanExecTime,
		detailFloat(t, f, "interval_mean_ms"))
}
