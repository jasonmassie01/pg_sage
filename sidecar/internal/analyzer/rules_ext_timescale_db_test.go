package analyzer

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// market-16b on real PostgreSQL. The local images do not carry TimescaleDB,
// so each test builds stub relations with the column shapes of TimescaleDB
// 2.x's timescaledb_information views (jobs, job_stats,
// continuous_aggregates, dimensions) and a SQL stub of cagg_watermark in
// _timescaledb_functions (2.12+) or _timescaledb_internal (earlier), and
// injects the gate that would have found the extension.
// No concurrent access tests: the extension reading belongs to one cycle on
// the analyzer's cycle goroutine.

const tsInfo = "timescaledb_information"

var tsWatermarkSchemas = []string{"_timescaledb_functions", "_timescaledb_internal"}

func tsAnalyzer(t *testing.T, schema string) (*Analyzer, *pgxpool.Pool, *extLog) {
	t.Helper()
	a, pool, logs := extAnalyzer(t, map[string]extInstall{
		extTimescale: {Schema: schema, Version: "2.17.2"}})
	for _, c := range tsCategories {
		cleanCategory(t, pool, c)
	}
	return a, pool, logs
}

func dropSchema(pool *pgxpool.Pool, q string) {
	_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
}

// tsStub creates TimescaleDB-shaped relations in schema, and cagg_watermark
// in each of wmSchemas, and returns the quoted schema.
func tsStub(t *testing.T, pool *pgxpool.Pool, schema string, wmSchemas ...string) string {
	t.Helper()
	q := sqlident.Ident(schema)
	all := append([]string{q}, tsWatermarkSchemas...)
	for _, s := range all {
		dropSchema(pool, s)
	}
	t.Cleanup(func() {
		for _, s := range all {
			dropSchema(pool, s)
		}
	})
	extExec(t, pool, "CREATE SCHEMA "+q)
	extExec(t, pool, `CREATE TABLE `+q+`.jobs (job_id integer PRIMARY KEY,
		application_name name, schedule_interval interval, max_runtime interval,
		max_retries integer, retry_period interval, proc_schema name, proc_name name,
		owner name, scheduled boolean, fixed_schedule boolean, config jsonb,
		next_start timestamptz, initial_start timestamptz, hypertable_schema name,
		hypertable_name name, check_schema name, check_name name)`)
	extExec(t, pool, `CREATE TABLE `+q+`.job_stats (hypertable_schema name,
		hypertable_name name, job_id integer, last_run_started_at timestamptz,
		last_successful_finish timestamptz, last_run_status text, job_status text,
		last_run_duration interval, next_start timestamptz, total_runs bigint,
		total_successes bigint, total_failures bigint)`)
	extExec(t, pool, `CREATE TABLE `+q+`.continuous_aggregates (hypertable_schema name,
		hypertable_name name, view_schema name, view_name name, view_owner name,
		materialized_only boolean, compression_enabled boolean,
		materialization_hypertable_schema name, materialization_hypertable_name name,
		view_definition text, finalized boolean)`)
	extExec(t, pool, `CREATE TABLE `+q+`.dimensions (hypertable_schema name,
		hypertable_name name, dimension_number bigint, column_name name,
		column_type regtype, dimension_type text, time_interval interval,
		integer_interval bigint, integer_now_func name, num_partitions smallint)`)
	extExec(t, pool, `CREATE TABLE `+q+`._stub_watermark (id integer PRIMARY KEY,
		wm bigint)`)
	for _, s := range wmSchemas {
		tsWatermarkFn(t, pool, q, s, 0)
	}
	return q
}

// tsWatermarkFn creates schema.cagg_watermark reading the stub table, with
// shift added to every watermark.
func tsWatermarkFn(t *testing.T, pool *pgxpool.Pool, q, schema string, shift int64) {
	t.Helper()
	extExec(t, pool, "CREATE SCHEMA IF NOT EXISTS "+schema)
	extExec(t, pool, `CREATE FUNCTION `+schema+`.cagg_watermark(hypertable_id integer)
		RETURNS bigint LANGUAGE sql STABLE AS $$
		SELECT wm + (`+strconv.FormatInt(shift, 10)+`) FROM `+q+`._stub_watermark
		WHERE id = hypertable_id $$`)
}

// tsJob adds a job and its stats; status "" stores a NULL last_run_status
// (a job that never ran).
func tsJob(t *testing.T, pool *pgxpool.Pool, q string, id int, proc, hypertable,
	status string, failures int64, scheduled bool) {
	t.Helper()
	var ht any
	if hypertable != "" {
		ht = hypertable
	}
	var st any
	if status != "" {
		st = status
	}
	extExec(t, pool, `INSERT INTO `+q+`.jobs (job_id, schedule_interval, proc_schema,
		proc_name, owner, scheduled, config, next_start, hypertable_schema, hypertable_name)
		VALUES ($1, '1 hour', '_timescaledb_functions', $2, current_user, $3, '{}',
		$4, CASE WHEN $5::text IS NULL THEN NULL ELSE 'public' END, $5)`,
		id, proc, scheduled, extTestNow.Add(30*time.Minute), ht)
	extExec(t, pool, `INSERT INTO `+q+`.job_stats (hypertable_schema, hypertable_name,
		job_id, last_run_started_at, last_run_status, job_status, next_start, total_runs,
		total_successes, total_failures)
		VALUES (CASE WHEN $2::text IS NULL THEN NULL ELSE 'public' END, $2, $1, $3, $4,
		'Scheduled', $5, $6 + 1, 1, $6)`,
		id, ht, extTestNow.Add(-time.Hour), st, extTestNow.Add(30*time.Minute), failures)
}

func tsSetStatus(t *testing.T, pool *pgxpool.Pool, q string, id int, status string) {
	t.Helper()
	extExec(t, pool, `UPDATE `+q+`.job_stats SET last_run_status = $2 WHERE job_id = $1`,
		id, status)
}

// tsCaggSpec is one continuous aggregate with its refresh policy.
type tsCaggSpec struct {
	view, schedule, endOffset, colType string
	job, mat                           int
	lag                                time.Duration
	scheduled                          bool
}

func tsAddCagg(t *testing.T, pool *pgxpool.Pool, q string, c tsCaggSpec) {
	t.Helper()
	ht, mat := "ht_"+c.view, "_materialized_hypertable_"+strconv.Itoa(c.mat)
	extExec(t, pool, `INSERT INTO `+q+`.continuous_aggregates (hypertable_schema,
		hypertable_name, view_schema, view_name, view_owner, materialized_only,
		materialization_hypertable_schema, materialization_hypertable_name)
		VALUES ('public', $1, 'public', $2, current_user, false, '_timescaledb_internal', $3)`,
		ht, c.view, mat)
	extExec(t, pool, `INSERT INTO `+q+`.dimensions (hypertable_schema, hypertable_name,
		dimension_number, column_name, column_type, dimension_type)
		VALUES ('public', $1, 1, 'ts', $2::regtype, 'Time')`, ht, c.colType)
	var off any
	if c.endOffset != "" {
		off = c.endOffset
	}
	extExec(t, pool, `INSERT INTO `+q+`.jobs (job_id, schedule_interval, proc_schema,
		proc_name, owner, scheduled, config, hypertable_schema, hypertable_name)
		VALUES ($1, $2::interval, '_timescaledb_functions',
		'policy_refresh_continuous_aggregate', current_user, $6,
		jsonb_build_object('mat_hypertable_id', $3::int, 'end_offset', $4::text,
		                   'start_offset', '7 days'), '_timescaledb_internal', $5)`,
		c.job, c.schedule, c.mat, off, mat, c.scheduled)
	extExec(t, pool, `INSERT INTO `+q+`.job_stats (hypertable_schema, hypertable_name,
		job_id, last_run_status, total_runs, total_successes, total_failures)
		VALUES ('_timescaledb_internal', $1, $2, 'Success', 1, 1, 0)`, mat, c.job)
	tsSetLag(t, pool, q, c.mat, c.lag)
}

func tsSetLag(t *testing.T, pool *pgxpool.Pool, q string, mat int, lag time.Duration) {
	t.Helper()
	tsSetWatermark(t, pool, q, mat, extTestNow.Add(-lag).UnixMicro())
}

func tsSetWatermark(t *testing.T, pool *pgxpool.Pool, q string, mat int, us int64) {
	t.Helper()
	extExec(t, pool, `INSERT INTO `+q+`._stub_watermark (id, wm) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET wm = EXCLUDED.wm`, mat, us)
}

func tsHourly(lag time.Duration) tsCaggSpec {
	return tsCaggSpec{view: "metrics_hourly", schedule: "1 hour", endOffset: "30 minutes",
		colType: "timestamptz", job: 1002, mat: 3, lag: lag, scheduled: true}
}

func TestTimescale_FailedCompressionJobGivesOneFinding(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsJob(t, pool, q, 1000, "policy_compression", "metrics", "Failed", 4, true)
	tsJob(t, pool, q, 1001, "policy_retention", "metrics", "Success", 0, true)
	got := byCategory(runExtCycle(t, a), categoryTSJobFailed)
	if len(got) != 1 {
		t.Fatalf("timescale_job_failed = %+v, want one", got)
	}
	d := got[0].Detail
	if got[0].ObjectIdentifier != "timescaledb:job:1000" || d["hypertable_name"] != "metrics" ||
		d["proc_name"] != "policy_compression" || d["total_failures"] != int64(4) ||
		d["next_start"] != extTestNow.Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("finding = %+v", got[0])
	}
	if n := countFindingRows(t, pool, categoryTSJobFailed, "timescaledb:job:1000",
		"open"); n != 1 {
		t.Fatalf("open rows = %d, want 1", n)
	}
	for _, c := range tsCategories {
		if !a.eval.ok[c] || a.eval.hasFailed(c) {
			t.Fatalf("%s not evaluated: %+v", c, a.eval)
		}
	}
}

func TestTimescale_NeverRunAndPausedJobsAreClear(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsJob(t, pool, q, 1003, "policy_compression", "metrics", "", 0, true)
	tsJob(t, pool, q, 1004, "policy_retention", "metrics", "Failed", 9, false)
	got := runExtCycle(t, a)
	if f := byCategory(got, categoryTSJobFailed); len(f) != 0 {
		t.Fatalf("a never-run or paused job gave %+v", f)
	}
	if !a.eval.ok[categoryTSJobFailed] || a.eval.hasFailed(categoryTSJobFailed) {
		t.Fatal("timescale_job_failed not evaluated")
	}
}

func TestTimescale_RecoveredJobResolves(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsJob(t, pool, q, 1005, "policy_compression", "metrics", "Failed", 2, true)
	runExtCycle(t, a)
	ident := "timescaledb:job:1005"
	if n := countFindingRows(t, pool, categoryTSJobFailed, ident, "open"); n != 1 {
		t.Fatalf("open rows = %d, want 1", n)
	}
	tsSetStatus(t, pool, q, 1005, "Success")
	if got := byCategory(runExtCycle(t, a), categoryTSJobFailed); len(got) != 0 {
		t.Fatalf("a job whose last run succeeded = %+v", got)
	}
	if n := countFindingRows(t, pool, categoryTSJobFailed, ident, "resolved"); n != 1 {
		t.Fatalf("resolved rows = %d, want 1", n)
	}
}

func TestTimescale_JobLimitKeepsTheRestOpen(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	for id := 1; id <= tsMaxJobs+1; id++ {
		tsJob(t, pool, q, id, "policy_compression", "metrics", "Failed", 1, true)
	}
	got := byCategory(runExtCycle(t, a), categoryTSJobFailed)
	if len(got) != tsMaxJobs {
		t.Fatalf("findings = %d, want %d", len(got), tsMaxJobs)
	}
	if !a.eval.hasFailed(categoryTSJobFailed) {
		t.Fatal("a truncated job list let timescale_job_failed resolve")
	}
}

func TestTimescale_CaggLagBoundary(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	limit := tsCaggLagFactor * 90 * time.Minute // 3 x (1 hour + 30 minutes)
	tsAddCagg(t, pool, q, tsHourly(limit))
	if got := byCategory(runExtCycle(t, a), categoryTSCaggStale); len(got) != 0 {
		t.Fatalf("lag exactly %v gave %+v", limit, got)
	}
	if !a.eval.ok[categoryTSCaggStale] || a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("timescale_cagg_stale not evaluated")
	}
	tsSetLag(t, pool, q, 3, limit+time.Second)
	got := byCategory(runExtCycle(t, a), categoryTSCaggStale)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.metrics_hourly" {
		t.Fatalf("lag %v + 1s gave %+v, want one", limit, got)
	}
	d := got[0].Detail
	if d["lag_seconds"] != int64((limit+time.Second)/time.Second) || d["job_id"] != int64(1002) ||
		d["threshold_seconds"] != int64(limit/time.Second) || d["end_offset"] != "00:30:00" ||
		d["schedule_interval"] != "01:00:00" || d["hypertable_name"] != "ht_metrics_hourly" {
		t.Fatalf("detail = %+v", d)
	}
	tsSetLag(t, pool, q, 3, time.Minute)
	runExtCycle(t, a)
	if n := countFindingRows(t, pool, categoryTSCaggStale, "public.metrics_hourly",
		"resolved"); n != 1 {
		t.Fatalf("resolved rows after the refresh caught up = %d, want 1", n)
	}
}

func TestTimescale_CaggNullEndOffsetIsZero(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	c := tsHourly(3*time.Hour + time.Second)
	c.endOffset = ""
	tsAddCagg(t, pool, q, c)
	got := byCategory(runExtCycle(t, a), categoryTSCaggStale)
	if len(got) != 1 || got[0].Detail["threshold_seconds"] != int64(3*3600) {
		t.Fatalf("a NULL end_offset cagg = %+v, want threshold 3 x 1 hour", got)
	}
}

func TestTimescale_CaggsNotJudged(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	integer := tsHourly(0)
	integer.view, integer.job, integer.mat, integer.colType = "int_agg", 2001, 11, "bigint"
	integer.endOffset = ""
	tsAddCagg(t, pool, q, integer)
	tsSetWatermark(t, pool, q, 11, 5) // integer time units, not microseconds
	paused := tsHourly(1000 * time.Hour)
	paused.view, paused.job, paused.mat, paused.scheduled = "paused_agg", 2002, 12, false
	tsAddCagg(t, pool, q, paused)
	empty := tsHourly(0)
	empty.view, empty.job, empty.mat = "empty_agg", 2003, 13
	tsAddCagg(t, pool, q, empty)
	tsSetWatermark(t, pool, q, 13, -210866803200000000) // never materialized
	extExec(t, pool, `INSERT INTO `+q+`.continuous_aggregates (hypertable_schema,
		hypertable_name, view_schema, view_name, materialization_hypertable_schema,
		materialization_hypertable_name) VALUES ('public', 'ht_x', 'public', 'no_policy',
		'_timescaledb_internal', '_materialized_hypertable_99')`)
	got := byCategory(runExtCycle(t, a), categoryTSCaggStale)
	if len(got) != 0 {
		t.Fatalf("integer, paused, never-materialized or policy-less caggs gave %+v", got)
	}
	if !a.eval.ok[categoryTSCaggStale] || a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("timescale_cagg_stale not evaluated")
	}
}

func TestTimescale_PreTwoTwelveWatermarkFunction(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_internal")
	tsAddCagg(t, pool, q, tsHourly(10*time.Hour))
	got := byCategory(runExtCycle(t, a), categoryTSCaggStale)
	if len(got) != 1 ||
		got[0].Detail["watermark_function"] != "_timescaledb_internal.cagg_watermark" {
		t.Fatalf("stale cagg through _timescaledb_internal = %+v", got)
	}
}

func TestTimescale_NewWatermarkFunctionWins(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo)
	tsWatermarkFn(t, pool, q, "_timescaledb_functions", 0)
	// A leftover old function reporting a far older watermark is not read.
	tsWatermarkFn(t, pool, q, "_timescaledb_internal", -1000*3600*1000000)
	tsAddCagg(t, pool, q, tsHourly(time.Hour))
	if got := byCategory(runExtCycle(t, a), categoryTSCaggStale); len(got) != 0 {
		t.Fatalf("the old watermark function was read: %+v", got)
	}
}

func TestTimescale_NoWatermarkFunctionSkipsOnlyCaggs(t *testing.T) {
	a, pool, logs := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo)
	tsAddCagg(t, pool, q, tsHourly(time.Hour))
	runExtCycle(t, a) // not evaluated, no finding: nothing to keep open yet
	tsJob(t, pool, q, 1006, "policy_compression", "metrics", "Failed", 1, true)
	got := runExtCycle(t, a)
	if f := byCategory(got, categoryTSJobFailed); len(f) != 1 {
		t.Fatalf("timescale_job_failed without a watermark function = %+v, want one", f)
	}
	if !a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("timescale_cagg_stale evaluated without a watermark function")
	}
	if a.eval.hasFailed(categoryTSJobFailed) {
		t.Fatal("timescale_job_failed failed with only the watermark function missing")
	}
	if n := logs.warns("cagg_watermark"); n != 1 {
		t.Fatalf("WARN lines naming cagg_watermark over a streak of 2 = %d, want 1", n)
	}
}

func TestTimescale_MissingWatermarkKeepsStaleFindingOpen(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsAddCagg(t, pool, q, tsHourly(100*time.Hour))
	runExtCycle(t, a)
	ident := "public.metrics_hourly"
	if n := countFindingRows(t, pool, categoryTSCaggStale, ident, "open"); n != 1 {
		t.Fatalf("open rows = %d, want 1", n)
	}
	extExec(t, pool, "DROP FUNCTION _timescaledb_functions.cagg_watermark(integer)")
	runExtCycle(t, a)
	if n := countFindingRows(t, pool, categoryTSCaggStale, ident, "open"); n != 1 {
		t.Fatalf("open rows after the function went away = %d, want 1 (left open)", n)
	}
}

func TestTimescale_OneXShapeIsNotEvaluated(t *testing.T) {
	a, pool, logs := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	extExec(t, pool, "DROP TABLE "+q+".jobs")
	extExec(t, pool, "DROP TABLE "+q+".job_stats")
	got := runExtCycle(t, a)
	for _, c := range tsCategories {
		if !a.eval.hasFailed(c) {
			t.Fatalf("%s evaluated without timescaledb_information.jobs", c)
		}
	}
	if len(byCategory(got, categoryTSJobFailed))+len(byCategory(got, categoryTSCaggStale)) != 0 {
		t.Fatalf("findings without the 2.x views: %+v", got)
	}
	if n := logs.warns("timescaledb_information"); n != 1 {
		t.Fatalf("WARN lines naming the views = %d, want 1", n)
	}
}

func TestTimescale_QuotedInformationSchema(t *testing.T) {
	schema := `My "tsdb"`
	a, pool, _ := tsAnalyzer(t, schema)
	q := tsStub(t, pool, schema, "_timescaledb_functions")
	tsJob(t, pool, q, 1007, "policy_compression", "metrics", "Failed", 1, true)
	tsAddCagg(t, pool, q, tsHourly(100*time.Hour))
	got := runExtCycle(t, a)
	if len(byCategory(got, categoryTSJobFailed)) != 1 ||
		len(byCategory(got, categoryTSCaggStale)) != 1 {
		t.Fatalf("findings with a quoted schema = %+v", got)
	}
}

func TestTimescale_RulePackResolvesRuleDisabled(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsJob(t, pool, q, 1008, "policy_compression", "metrics", "Failed", 1, true)
	runExtCycle(t, a)
	a.SetDisabledRules([]string{categoryTSJobFailed}, "ops@example.test")
	if got := byCategory(runExtCycle(t, a), categoryTSJobFailed); len(got) != 0 {
		t.Fatalf("a disabled rule emitted %+v", got)
	}
	ident := "timescaledb:job:1008"
	if v := storedDetail(t, pool, categoryTSJobFailed, ident, "resolution"); v !=
		RuleDisabledResolution {
		t.Fatalf("resolution = %q, want %s", v, RuleDisabledResolution)
	}
}

func TestTimescale_BothRulesDisabledReadsOnlyTheGate(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	tsStub(t, pool, tsInfo, "_timescaledb_functions")
	a.SetDisabledRules(append([]string{}, tsCategories...), "ops@example.test")
	a.eval = newCycleEval()
	var n int
	a.readExtensions(context.Background(), countingReader(pool, &n))
	if n != 0 {
		t.Fatalf("statements with both rules disabled = %d, want 0 (the gate is injected)", n)
	}
	if a.extras.ext == nil || a.extras.ext.timescale != nil {
		t.Fatalf("timescale reading with both rules disabled = %+v", a.extras.ext)
	}
}

func TestTimescale_NoExtensionRunsOnlyTheGate(t *testing.T) {
	pool := phase2Pool(t)
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	var n int
	a.readExtensions(context.Background(), countingReader(pool, &n))
	if n != 1 {
		t.Fatalf("statements without TimescaleDB = %d, want 1 (the gate)", n)
	}
	for _, c := range tsCategories {
		if !a.eval.hasFailed(c) {
			t.Errorf("%s evaluated without TimescaleDB", c)
		}
	}
	if got := ruleTimescaleJobFailed(nil, nil, nil, a.extras); len(got) != 0 {
		t.Fatalf("no extension emitted %+v", got)
	}
}
