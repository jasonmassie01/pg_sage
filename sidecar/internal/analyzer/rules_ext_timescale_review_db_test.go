package analyzer

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-16b review: the watermark is the end of the newest materialized
// bucket, so it stops when the source stops receiving rows even while the
// refresh policy keeps succeeding; and TimescaleDB's own telemetry job is
// not an operator's policy. On the stubs of rules_ext_timescale_db_test.go.
// No concurrent access tests: the extension reading belongs to one cycle on
// the analyzer's cycle goroutine.

func tsSetLastSuccess(t *testing.T, pool *pgxpool.Pool, q string, job int, at time.Time) {
	t.Helper()
	extExec(t, pool, `UPDATE `+q+`.job_stats SET last_successful_finish = $2
		WHERE job_id = $1`, job, at)
}

func TestTimescale_CaggWithSucceedingPolicyIsClear(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	limit := tsCaggLagFactor * 90 * time.Minute
	tsAddCagg(t, pool, q, tsHourly(100*time.Hour)) // the source went quiet
	tsSetLastSuccess(t, pool, q, 1002, extTestNow.Add(-limit))
	if got := byCategory(runExtCycle(t, a), categoryTSCaggStale); len(got) != 0 {
		t.Fatalf("a policy that succeeded %v ago gave %+v", limit, got)
	}
	if !a.eval.ok[categoryTSCaggStale] || a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("timescale_cagg_stale not evaluated")
	}
	last := extTestNow.Add(-limit - time.Second)
	tsSetLastSuccess(t, pool, q, 1002, last)
	got := byCategory(runExtCycle(t, a), categoryTSCaggStale)
	if len(got) != 1 || got[0].Detail["last_successful_finish"] != last.Format(time.RFC3339) {
		t.Fatalf("a policy that last succeeded %v + 1s ago gave %+v, want one", limit, got)
	}
	tsSetLastSuccess(t, pool, q, 1002, extTestNow.Add(-time.Minute))
	runExtCycle(t, a)
	if n := countFindingRows(t, pool, categoryTSCaggStale, "public.metrics_hourly",
		"resolved"); n != 1 {
		t.Fatalf("resolved rows after the policy succeeded = %d, want 1", n)
	}
}

func TestTimescale_CaggNeverSucceededIsJudgedByWatermark(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsAddCagg(t, pool, q, tsHourly(100*time.Hour)) // job_stats has no finish
	got := byCategory(runExtCycle(t, a), categoryTSCaggStale)
	if len(got) != 1 {
		t.Fatalf("a policy that never succeeded gave %+v, want one", got)
	}
	if _, ok := got[0].Detail["last_successful_finish"]; ok {
		t.Fatalf("detail names a success that never happened: %+v", got[0].Detail)
	}
}

func TestTimescale_UnreadableJobStatsJudgesByWatermarkAlone(t *testing.T) {
	a, pool, logs := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsAddCagg(t, pool, q, tsHourly(100*time.Hour))
	tsSetLastSuccess(t, pool, q, 1002, extTestNow.Add(-time.Minute))
	role := tsLimitedRole(t, pool, q)
	readAs(t, a, pool, role)
	if a.eval.hasFailed(categoryTSCaggStale) {
		t.Fatal("timescale_cagg_stale failed: it judges by the watermark without job_stats")
	}
	if got := ruleTimescaleCaggStale(nil, nil, nil, a.extras); len(got) != 1 {
		t.Fatalf("stale cagg without job_stats = %+v, want one (watermark alone)", got)
	}
	if n := logs.warns("job_stats"); n != 1 {
		t.Fatalf("WARN lines naming job_stats = %d, want 1 (the jobs read only)", n)
	}
}

func TestTimescale_TelemetryJobIsNotReported(t *testing.T) {
	a, pool, _ := tsAnalyzer(t, tsInfo)
	q := tsStub(t, pool, tsInfo, "_timescaledb_functions")
	tsJob(t, pool, q, 1, "policy_telemetry", "", "Failed", 30, true)
	tsJob(t, pool, q, 2, "policy_telemetry", "", "Failed", 30, true)
	extExec(t, pool, `UPDATE `+q+`.jobs SET proc_schema = '_timescaledb_internal'
		WHERE job_id = 2`)
	tsJob(t, pool, q, 1011, "policy_telemetry", "", "Failed", 1, true)
	extExec(t, pool, `UPDATE `+q+`.jobs SET proc_schema = 'public' WHERE job_id = 1011`)
	got := byCategory(runExtCycle(t, a), categoryTSJobFailed)
	if len(got) != 1 || got[0].ObjectIdentifier != "timescaledb:job:1011" {
		t.Fatalf("timescale_job_failed = %+v, want only the user action 1011", got)
	}
}
