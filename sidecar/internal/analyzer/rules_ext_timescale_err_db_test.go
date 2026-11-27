package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-16b's failure paths on real PostgreSQL, on the stubs of
// rules_ext_timescale_db_test.go.
// No concurrent access tests: the extension reading belongs to one cycle on
// the analyzer's cycle goroutine.

// tsLimitedRole is a role that reads every stub relation but job_stats.
func tsLimitedRole(t *testing.T, pool *pgxpool.Pool, q string) string {
	t.Helper()
	role := "sage_ts_reader"
	drop := func() {
		_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	}
	drop()
	extExec(t, pool, "CREATE ROLE "+role+" NOLOGIN")
	t.Cleanup(drop)
	extExec(t, pool, "GRANT USAGE ON SCHEMA "+q+", _timescaledb_functions TO "+role)
	for _, tbl := range []string{"jobs", "continuous_aggregates", "dimensions",
		"_stub_watermark"} {
		extExec(t, pool, "GRANT SELECT ON "+q+"."+tbl+" TO "+role)
	}
	return role
}

func TestTimescale_PermissionDeniedOnJobStats(t *testing.T) {
	a, pool, logs := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsJob(t, pool, q, 1009, "policy_compression", "metrics", "Failed", 3, true)
	tsAddCagg(t, pool, q, tsHourly(100*time.Hour))
	runExtCycle(t, a) // opens both findings as a privileged reader
	role := tsLimitedRole(t, pool, q)

	readAs(t, a, pool, role)
	if !a.eval.hasFailed(categoryTSJobFailed) {
		t.Fatal("timescale_job_failed evaluated without SELECT on job_stats")
	}
	if a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("timescale_cagg_stale failed: it does not read job_stats")
	}
	if got := ruleTimescaleJobFailed(nil, nil, nil, a.extras); len(got) != 0 {
		t.Fatalf("an unread job list emitted %+v", got)
	}
	got := ruleTimescaleCaggStale(nil, nil, nil, a.extras)
	if len(got) != 1 {
		t.Fatalf("stale cagg as the limited role = %+v, want one", got)
	}
	if err := a.finalizeCycle(context.Background(), got,
		a.eval.resolvable(got)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if n := countFindingRows(t, pool, categoryTSJobFailed, "timescaledb:job:1009",
		"open"); n != 1 {
		t.Fatalf("open rows after a denied read = %d, want 1 (left open)", n)
	}
	readAs(t, a, pool, role)
	if n := logs.warns("job_stats"); n != 1 {
		t.Fatalf("WARN lines naming job_stats over a streak of 2 = %d, want 1", n)
	}
	extExec(t, pool, "GRANT SELECT ON "+q+".job_stats TO "+role)
	readAs(t, a, pool, role)
	if a.eval.hasFailed(categoryTSJobFailed) {
		t.Fatal("timescale_job_failed failed after the grant")
	}
}

func TestTimescale_MissingAggregateViewsSkipOnlyCaggs(t *testing.T) {
	a, pool, logs := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsJob(t, pool, q, 1010, "policy_retention", "metrics", "Failed", 1, true)
	extExec(t, pool, "DROP TABLE "+q+".dimensions")
	got := runExtCycle(t, a)
	if len(byCategory(got, categoryTSJobFailed)) != 1 {
		t.Fatalf("timescale_job_failed without the aggregate views = %+v", got)
	}
	if !a.eval.hasFailed(categoryTSCaggStale) || a.eval.hasFailed(categoryTSJobFailed) {
		t.Fatalf("evaluation without the aggregate views = %+v", a.eval)
	}
	if n := logs.warns("dimensions"); n != 1 {
		t.Fatalf("WARN lines naming dimensions = %d, want 1", n)
	}
}

func TestTimescale_CaggQueryErrorIsNotEvaluated(t *testing.T) {
	a, pool, logs := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	c := tsHourly(time.Hour)
	c.endOffset = "not an interval"
	tsAddCagg(t, pool, q, c)
	runExtCycle(t, a)
	if !a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("timescale_cagg_stale evaluated after its query failed")
	}
	if n := logs.warns("watermarks"); n != 1 {
		t.Fatalf("WARN lines for the failed aggregate read = %d, want 1", n)
	}
}

func TestTimescale_CaggLimitKeepsTheRestOpen(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	n := tsMaxCaggs + 1
	extExec(t, pool, `INSERT INTO `+q+`.continuous_aggregates (hypertable_schema,
		hypertable_name, view_schema, view_name, materialization_hypertable_schema,
		materialization_hypertable_name)
		SELECT 'public', 'ht', 'public', format('agg_%s', lpad(g::text, 3, '0')),
		       '_timescaledb_internal', 'mat_' || g FROM generate_series(1, $1) g`, n)
	extExec(t, pool, `INSERT INTO `+q+`.dimensions (hypertable_schema, hypertable_name,
		dimension_number, column_name, column_type)
		VALUES ('public', 'ht', 1, 'ts', 'timestamptz')`)
	extExec(t, pool, `INSERT INTO `+q+`.jobs (job_id, schedule_interval, proc_name,
		scheduled, config, hypertable_schema, hypertable_name)
		SELECT 3000 + g, '1 hour', 'policy_refresh_continuous_aggregate', true,
		       jsonb_build_object('mat_hypertable_id', g, 'end_offset', '1 hour'),
		       '_timescaledb_internal', 'mat_' || g FROM generate_series(1, $1) g`, n)
	extExec(t, pool, `INSERT INTO `+q+`._stub_watermark (id, wm)
		SELECT g, $2 FROM generate_series(1, $1) g`, n,
		extTestNow.Add(-100*time.Hour).UnixMicro())
	got := byCategory(runExtCycle(t, a), categoryTSCaggStale)
	if len(got) != tsMaxCaggs {
		t.Fatalf("stale aggregates = %d, want %d", len(got), tsMaxCaggs)
	}
	if !a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("a truncated aggregate list let timescale_cagg_stale resolve")
	}
}
