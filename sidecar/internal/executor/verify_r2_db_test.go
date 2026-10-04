package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Dogfood round 2 (item 4), on real Postgres: REINDEX and CREATE
// STATISTICS run through the post-action monitor and land a verdict in
// sage.action_outcome, which the trust ledger counts.

func r2MonitorConfig() RollbackMonitorConfig {
	return RollbackMonitorConfig{ThresholdPct: 10, WindowMinutes: 15, CapMinutes: 4320,
		StatementTimeout: time.Minute, LockTimeoutMs: 1500, Authorize: allowRollback}
}

// withTarget points a prediction recorded by predictAction at one
// synthetic target with a frozen baseline (the test seeds its samples).
func withTarget(t *testing.T, before map[string]any, qid int64) string {
	t.Helper()
	p, ok := predictionIn(before)
	if !ok {
		t.Fatalf("predictAction recorded no prediction: %+v", before)
	}
	p.TargetQueryIDs = []int64{qid}
	before["predicted_effect"] = p
	before["target_queryids"] = []int64{qid}
	before["verify_baseline"] = map[string]verify.Measurement{
		strconv.FormatInt(qid, 10): {Samples: 600,
			AverageLatency: time.Duration(fixtureBaselineMs * float64(time.Millisecond)),
			StdErr:         50 * time.Microsecond, Buckets: 30}}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("encode before_state: %v", err)
	}
	return string(raw)
}

func bloatedIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string,
	bloat bool) string {
	t.Helper()
	index := table + "_a"
	stmts := []string{"CREATE INDEX " + index + " ON public." + table + " (a)",
		"INSERT INTO public." + table + " SELECT i, i FROM generate_series(1, 40000) i"}
	if bloat {
		stmts = append(stmts, "DELETE FROM public."+table+" WHERE a % 10 <> 0",
			"VACUUM public."+table)
	}
	for _, sql := range stmts {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return "public." + index
}

func reindexAction(t *testing.T, bloat bool, meanAfterMs float64) (*Executor, int64,
	context.Context) {
	t.Helper()
	table, exec, ctx := verifiedTable(t, "public")
	index := bloatedIndex(t, ctx, exec.pool, table, bloat)
	sql := "REINDEX INDEX CONCURRENTLY " + index
	before := map[string]any{}
	exec.predictAction(ctx, sql, map[string]any{}, before)
	if before["reindex_bytes"] == nil {
		t.Fatalf("predictAction recorded no index size: %+v", before)
	}
	if _, err := exec.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	qid := int64(8_720_000_000) + time.Now().UnixNano()%1_000_000
	executedAt := time.Now().Add(-2 * time.Hour).UTC()
	seedQuerySamples(t, ctx, exec.pool, qid, executedAt.Add(time.Second), 110, 40,
		meanAfterMs)
	id := insertVerifiedAction(t, exec.pool, verifiedActionRow{sql: sql,
		before: withTarget(t, before, qid), executedAt: executedAt,
		decisionID: insertParkDecision(t, ctx, exec.pool)})
	return exec, id, ctx
}

func TestReindexThatReclaimsBloatIsImproved(t *testing.T) {
	exec, id, ctx := reindexAction(t, true, fixtureBaselineMs)
	MonitorAndRollback(ctx, exec.pool, id, "", r2MonitorConfig(), nopLog, nil)

	got := storedOutcome(t, exec.pool, id)
	if got.Class != verify.ClassReindex || got.Verdict != verify.OutcomeImproved {
		t.Fatalf("outcome = %s/%s (%s), want reindex/improved", got.Class, got.Verdict,
			got.Reason)
	}
	if got.Observed.Metric != verify.MetricIndexBytes || got.Observed.After >=
		got.Observed.Before*0.9 {
		t.Fatalf("observed = %+v, want the index at least 10%% smaller", got.Observed)
	}
	if outcome, _ := actionOutcomeFor(t, exec.pool, id); outcome != "success" {
		t.Fatalf("action outcome = %q, want success", outcome)
	}
	if v := verificationVerdict(t, exec.pool, id); v != "success" {
		t.Fatalf("verification verdict = %q, want success", v)
	}
}

// A REINDEX of a tight index reclaims nothing: neutral, no harm. Hygiene
// counts a held neutral; the verdict itself must stay neutral.
func TestReindexOfATightIndexIsNeutral(t *testing.T) {
	exec, id, ctx := reindexAction(t, false, fixtureBaselineMs)
	MonitorAndRollback(ctx, exec.pool, id, "", r2MonitorConfig(), nopLog, nil)
	got := storedOutcome(t, exec.pool, id)
	if got.Verdict != verify.OutcomeNeutral {
		t.Fatalf("outcome = %s (%s), want neutral", got.Verdict, got.Reason)
	}
	if outcome, _ := actionOutcomeFor(t, exec.pool, id); outcome != "success" {
		t.Fatalf("action outcome = %q, want success (kept, measured)", outcome)
	}
}

// REINDEX has no rollback: a regression of the table's queries is
// recorded as regressed (the ledger's demerit) and nothing is executed.
func TestReindexRegressionIsRecordedWithoutARollback(t *testing.T) {
	exec, id, ctx := reindexAction(t, true, fixtureBaselineMs*3)
	executed := 0
	cfg := r2MonitorConfig()
	cfg.execRollback = func(context.Context, string, time.Duration, ...DDLOption) error {
		executed++
		return nil
	}
	MonitorAndRollback(ctx, exec.pool, id, "", cfg, nopLog, nil)
	got := storedOutcome(t, exec.pool, id)
	if got.Verdict != verify.OutcomeRegressed {
		t.Fatalf("outcome = %s (%s), want regressed", got.Verdict, got.Reason)
	}
	if executed != 0 {
		t.Fatalf("a rollback statement ran %d times for a REINDEX", executed)
	}
	if outcome, _ := actionOutcomeFor(t, exec.pool, id); outcome != "rollback_skipped" {
		t.Fatalf("action outcome = %q, want rollback_skipped", outcome)
	}
	rb, _ := got.Evidence["rollback"].(map[string]any)
	if rb["restored"] != false {
		t.Fatalf("rollback evidence = %+v, want restored=false", got.Evidence)
	}
}

// statisticsAction captures plans of a correlated two-column predicate
// before CREATE STATISTICS, records the action like the executor does,
// runs it with its ANALYZE, then captures plans after.
func statisticsAction(t *testing.T, meanAfterMs float64) (*Executor, int64,
	context.Context) {
	t.Helper()
	table, exec, ctx := verifiedTable(t, "public")
	pool := exec.pool
	for _, sql := range []string{"INSERT INTO public." + table +
		" SELECT i % 100, i % 100 FROM generate_series(1, 20000) i",
		"ANALYZE public." + table} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	qid := int64(8_730_000_000) + time.Now().UnixNano()%1_000_000
	query := fmt.Sprintf("SELECT * FROM public.%s WHERE a = 1 AND b = 1", table)
	capture := func(at time.Time) {
		for i := 0; i < 3; i++ {
			var plan string
			if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query).
				Scan(&plan); err != nil {
				t.Fatalf("explain: %v", err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at,
				queryid, query_text, plan_json, source)
				VALUES ($1, $2, $3, $4::jsonb, 'auto_explain_log')`,
				at.Add(time.Duration(i)*time.Millisecond), qid, query, plan); err != nil {
				t.Fatalf("store plan: %v", err)
			}
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
	})
	executedAt := time.Now().Add(-2 * time.Hour).UTC()
	capture(executedAt.Add(-time.Hour))
	sql := "CREATE STATISTICS " + table + "_ab (dependencies) ON a, b FROM public." + table
	before := map[string]any{}
	exec.predictAction(ctx, sql, map[string]any{"queryid": float64(qid)}, before)
	for _, s := range []string{sql, "ANALYZE public." + table} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	waitAnalyzed(t, ctx, pool, "public."+table)
	capture(time.Now().UTC())
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 110, 40, meanAfterMs)
	id := insertVerifiedAction(t, pool, verifiedActionRow{sql: sql,
		before: withTarget(t, before, qid), executedAt: executedAt})
	return exec, id, ctx
}

// waitAnalyzed waits for the statistics views to show the ANALYZE (PG14
// reports through the collector).
func waitAnalyzed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		var done bool
		if err := pool.QueryRow(ctx, `SELECT COALESCE(last_analyze > now() - interval
			'1 minute', false) FROM pg_stat_user_tables WHERE relid = to_regclass($1)`,
			table).Scan(&done); err == nil && done {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("ANALYZE of %s never showed in pg_stat_user_tables", table)
}

func TestStatisticsThatFixEstimatesAreImproved(t *testing.T) {
	exec, id, ctx := statisticsAction(t, fixtureBaselineMs)
	MonitorAndRollback(ctx, exec.pool, id, "", r2MonitorConfig(), nopLog, nil)
	got := storedOutcome(t, exec.pool, id)
	if got.Class != verify.ClassStatistics || got.Verdict != verify.OutcomeImproved {
		t.Fatalf("outcome = %s/%s (%s), want statistics/improved", got.Class, got.Verdict,
			got.Reason)
	}
	if got.Observed.Metric != verify.MetricRowEstimateError || got.Observed.Before < 20 ||
		got.Observed.After > 2 {
		t.Fatalf("observed = %+v, want the ~100x row-estimate error gone", got.Observed)
	}
	if got.Tolerance != verify.ToleranceMet {
		t.Fatalf("tolerance = %s, want met (predicted -50%%)", got.Tolerance)
	}
	if outcome, _ := actionOutcomeFor(t, exec.pool, id); outcome != "success" {
		t.Fatalf("action outcome = %q, want success", outcome)
	}
}

func TestStatisticsWithRegressedQueriesAreRegressed(t *testing.T) {
	exec, id, ctx := statisticsAction(t, fixtureBaselineMs*3)
	MonitorAndRollback(ctx, exec.pool, id, "", r2MonitorConfig(), nopLog, nil)
	got := storedOutcome(t, exec.pool, id)
	if got.Verdict != verify.OutcomeRegressed {
		t.Fatalf("outcome = %s (%s), want regressed", got.Verdict, got.Reason)
	}
}

// A restart must resume the watch of a REINDEX, which has no rollback SQL
// (before, only actions with one were resumed).
func TestResumeOrphanedMonitorsIncludesReindex(t *testing.T) {
	exec, id, ctx := reindexAction(t, true, fixtureBaselineMs)
	if err := exec.resumeOrphanedMonitors(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Other leftover actions may be resumed too: wait for this one only,
	// then stop every monitor.
	deadline := time.Now().Add(60 * time.Second)
	var got verify.Outcome
	for time.Now().Before(deadline) {
		o, err := verify.NewOutcomeStore(exec.pool).Get(ctx, id)
		if err == nil && verify.DecidedVerdict(o.Verdict) {
			got = o
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.Shutdown(shutdownCtx)
	if got.Verdict != verify.OutcomeImproved {
		t.Fatalf("resumed REINDEX outcome = %q (%s), want improved", got.Verdict,
			got.Reason)
	}
}
