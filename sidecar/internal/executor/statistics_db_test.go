package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Owner decision 2026-10-04 (PR #110), end to end on real PostgreSQL:
// an operator-approved CREATE STATISTICS runs with its ANALYZE as one
// action, the statistics verifier judges it from that ANALYZE, and the
// recorded rollback drops exactly the created object (by hand, or by the
// monitor on a regression).

type statsFixture struct {
	exec      *Executor
	ctx       context.Context
	pool      *pgxpool.Pool
	table     string // unqualified
	name      string // statistics object, unqualified
	sql       string
	findingID int
	qid       int64
	query     string
}

func newStatsFixture(t *testing.T) *statsFixture {
	t.Helper()
	table, exec, ctx := verifiedTable(t, "public")
	fx := &statsFixture{exec: exec, ctx: ctx, pool: exec.pool, table: table,
		name: "sage_stx_" + table + "_ab",
		qid:  int64(8_740_000_000) + time.Now().UnixNano()%1_000_000}
	fx.sql = fmt.Sprintf("CREATE STATISTICS public.%s (dependencies, ndistinct) "+
		"ON a, b FROM public.%s", fx.name, table)
	fx.query = fmt.Sprintf("SELECT * FROM public.%s WHERE a = 1 AND b = 1", table)
	for _, sql := range []string{"INSERT INTO public." + table +
		" SELECT i % 100, i % 100 FROM generate_series(1, 20000) i",
		"ANALYZE public." + table} {
		fx.mustExec(t, sql)
	}
	exec.cfg.Trust.Level = "advisory"
	withTestStandingGate(exec)
	detail, _ := json.Marshal(map[string]any{"queryid": float64(fx.qid)})
	if err := fx.pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		VALUES ('query_create_statistics', 'warning', 'table', $1, 'correlated a,b',
		$2::jsonb, 'create statistics', $3) RETURNING id`, "public."+table, string(detail),
		fx.sql).Scan(&fx.findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() { fx.cleanup() })
	return fx
}

func (fx *statsFixture) cleanup() {
	ctx := context.Background()
	shutdown, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = fx.exec.Shutdown(shutdown)
	_, _ = fx.pool.Exec(ctx, `UPDATE sage.action_log SET verification_id = NULL
		WHERE finding_id = $1`, fx.findingID)
	_, _ = fx.pool.Exec(ctx, `DELETE FROM sage.verification WHERE action_log_id IN
		(SELECT id FROM sage.action_log WHERE finding_id = $1)`, fx.findingID)
	_, _ = fx.pool.Exec(ctx, `DELETE FROM sage.action_outcome WHERE action_log_id IN
		(SELECT id FROM sage.action_log WHERE finding_id = $1)`, fx.findingID)
	_, _ = fx.pool.Exec(ctx, "DELETE FROM sage.action_log WHERE finding_id = $1", fx.findingID)
	_, _ = fx.pool.Exec(ctx, "DELETE FROM sage.findings WHERE id = $1", fx.findingID)
	_, _ = fx.pool.Exec(ctx, "DELETE FROM sage.explain_cache WHERE queryid = $1", fx.qid)
	_, _ = fx.pool.Exec(ctx, "DROP STATISTICS IF EXISTS public."+fx.name)
}

func (fx *statsFixture) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// capturePlans stores three EXPLAIN ANALYZE plans of the correlated query
// captured at at, as auto_explain would.
func (fx *statsFixture) capturePlans(t *testing.T, at time.Time) {
	t.Helper()
	for i := 0; i < 3; i++ {
		var plan string
		if err := fx.pool.QueryRow(fx.ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+fx.query).
			Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		fx.mustExec(t, `INSERT INTO sage.explain_cache (captured_at, queryid, query_text,
			plan_json, source) VALUES ($1, $2, $3, $4::jsonb, 'auto_explain_log')`,
			at.Add(time.Duration(i)*time.Millisecond), fx.qid, fx.query, plan)
	}
}

// run executes the finding as an operator would, with no monitor started
// (the test drives the monitor itself).
func (fx *statsFixture) run(t *testing.T, rollbackSQL string) (int64, error) {
	t.Helper()
	shutdown, cancel := context.WithTimeout(fx.ctx, 10*time.Second)
	defer cancel()
	if err := fx.exec.Shutdown(shutdown); err != nil {
		t.Fatalf("stop monitors: %v", err)
	}
	return fx.exec.ExecuteManual(fx.ctx, fx.findingID, fx.sql, rollbackSQL, nil)
}

func (fx *statsFixture) statisticsExist(t *testing.T) bool {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM pg_statistic_ext s
		JOIN pg_namespace n ON n.oid = s.stxnamespace
		WHERE n.nspname = 'public' AND s.stxname = $1`, fx.name).Scan(&n); err != nil {
		t.Fatalf("read pg_statistic_ext: %v", err)
	}
	return n == 1
}

// statisticsBuilt reports whether ANALYZE filled the object's data.
func (fx *statsFixture) statisticsBuilt(t *testing.T) bool {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM pg_statistic_ext_data d
		JOIN pg_statistic_ext s ON s.oid = d.stxoid
		WHERE s.stxname = $1 AND d.stxddependencies IS NOT NULL
		  AND d.stxdndistinct IS NOT NULL`, fx.name).Scan(&n); err != nil {
		t.Fatalf("read pg_statistic_ext_data: %v", err)
	}
	return n >= 1
}

type statsActionRow struct {
	id                   int64
	label, rollback, sql string
	outcome              string
	before               map[string]any
}

func (fx *statsFixture) actions(t *testing.T) []statsActionRow {
	t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT id, action_type, COALESCE(rollback_sql, ''),
		sql_executed, outcome, COALESCE(before_state, '{}'::jsonb)
		FROM sage.action_log WHERE finding_id = $1 ORDER BY id`, fx.findingID)
	if err != nil {
		t.Fatalf("read actions: %v", err)
	}
	defer rows.Close()
	var out []statsActionRow
	for rows.Next() {
		var r statsActionRow
		var raw []byte
		if err := rows.Scan(&r.id, &r.label, &r.rollback, &r.sql, &r.outcome, &raw); err != nil {
			t.Fatalf("scan action: %v", err)
		}
		if err := json.Unmarshal(raw, &r.before); err != nil {
			t.Fatalf("decode before_state: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestCreateStatisticsRunsWithItsAnalyzeAsOneAction(t *testing.T) {
	fx := newStatsFixture(t)
	id, err := fx.run(t, "")
	if err != nil {
		t.Fatalf("ExecuteManual: %v", err)
	}
	if !fx.statisticsExist(t) {
		t.Fatal("statistics object was not created")
	}
	if !fx.statisticsBuilt(t) {
		t.Fatal("statistics object is empty: the action did not ANALYZE the table")
	}
	acts := fx.actions(t)
	if len(acts) != 1 || acts[0].id != id {
		t.Fatalf("action_log rows for the finding = %+v, want exactly action %d", acts, id)
	}
	a := acts[0]
	if a.label != "create_statistics" || a.sql != fx.sql {
		t.Fatalf("action = %s %q, want create_statistics %q", a.label, a.sql, fx.sql)
	}
	if want := "DROP STATISTICS IF EXISTS public." + fx.name; a.rollback != want {
		t.Fatalf("rollback = %q, want %q", a.rollback, want)
	}
	if _, ok := a.before["statistics_analyze_mark"]; !ok {
		t.Fatalf("before_state lacks the pre-action analyze mark: %v", a.before)
	}
	var analyzeRows int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed ILIKE 'ANALYZE %' AND sql_executed ILIKE $1`,
		"%"+fx.table+"%").Scan(&analyzeRows); err != nil {
		t.Fatalf("count analyze actions: %v", err)
	}
	if analyzeRows != 0 {
		t.Fatalf("%d separate ANALYZE actions recorded, want 0 (one action)", analyzeRows)
	}
}

// The verifier must count the action's own ANALYZE as the build of the
// statistics, although action_log.executed_at is stamped after it.
func TestStatisticsVerdictFromTheActionsOwnAnalyze(t *testing.T) {
	fx := newStatsFixture(t)
	fx.capturePlans(t, time.Now().Add(-time.Hour).UTC())
	id, err := fx.run(t, "")
	if err != nil {
		t.Fatalf("ExecuteManual: %v", err)
	}
	plan, err := loadMonitorPlan(fx.ctx, fx.pool, id, r2MonitorConfig())
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	waitBuiltSinceAction(t, fx.ctx, fx.pool, plan)
	time.Sleep(20 * time.Millisecond)
	fx.capturePlans(t, time.Now().UTC())
	evidence := map[string]any{}
	j := plan.judgeEstimates(fx.ctx, fx.pool, time.Now().Add(time.Second), evidence)
	if j.Verdict != verify.OutcomeImproved {
		t.Fatalf("estimates = %s (%s), want improved from the in-action ANALYZE",
			j.Verdict, j.Reason)
	}
}

// waitBuiltSinceAction waits until the table's analyze time is past the
// action's own mark, as the verifier requires. Waiting for "any ANALYZE in
// the last minute" also accepted the fixture's setup ANALYZE, and on PG14
// the collector reports the action's ANALYZE up to ~500 ms later (CI:
// "statistics not built yet").
func waitBuiltSinceAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	plan monitorPlan) {
	t.Helper()
	for i := 0; i < 40; i++ {
		var built *time.Time
		if err := pool.QueryRow(ctx, statisticsBuiltSQL, plan.r2.StatisticsTable).
			Scan(&built); err == nil && plan.builtSinceAction(built) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the action's ANALYZE of %s never showed in pg_stat_user_tables",
		plan.r2.StatisticsTable)
}

// improvedThenRolledBack: the full monitor records an improved statistics
// verdict, then the operator's rollback drops exactly the object.
func TestStatisticsImprovedThenRolledBackByHand(t *testing.T) {
	fx := newStatsFixture(t)
	id := fx.monitoredAction(t, fixtureBaselineMs)
	ctx, cancel := context.WithTimeout(fx.ctx, 2*time.Minute)
	defer cancel()
	MonitorAndRollback(ctx, fx.pool, id, "DROP STATISTICS IF EXISTS public."+fx.name,
		r2MonitorConfig(), nopLog, nil)
	got := storedOutcome(t, fx.pool, id)
	if got.Class != verify.ClassStatistics || got.Verdict != verify.OutcomeImproved {
		t.Fatalf("outcome = %s/%s (%s), want statistics/improved", got.Class, got.Verdict,
			got.Reason)
	}
	if !fx.statisticsExist(t) {
		t.Fatal("an improved action was rolled back")
	}
	if err := fx.exec.RollbackAction(fx.ctx, id, "operator test"); err != nil {
		t.Fatalf("RollbackAction: %v", err)
	}
	if fx.statisticsExist(t) {
		t.Fatal("rollback left the statistics object behind")
	}
	if outcome, _ := actionOutcomeFor(t, fx.pool, id); outcome != "rolled_back" {
		t.Fatalf("action outcome = %q, want rolled_back", outcome)
	}
}

// A regression is undone by the monitor with the recorded rollback.
func TestStatisticsRegressionIsRolledBack(t *testing.T) {
	fx := newStatsFixture(t)
	id := fx.monitoredAction(t, fixtureBaselineMs*3)
	cfg := r2MonitorConfig()
	MonitorAndRollback(fx.ctx, fx.pool, id, "DROP STATISTICS IF EXISTS public."+fx.name,
		cfg, nopLog, nil)
	got := storedOutcome(t, fx.pool, id)
	if got.Verdict != verify.OutcomeRegressed {
		t.Fatalf("outcome = %s (%s), want regressed", got.Verdict, got.Reason)
	}
	if fx.statisticsExist(t) {
		t.Fatal("regressed statistics were not dropped")
	}
	if outcome, _ := actionOutcomeFor(t, fx.pool, id); outcome != "rolled_back" {
		t.Fatalf("action outcome = %q, want rolled_back", outcome)
	}
}

// monitoredAction runs the action through ExecuteManual, then makes it a
// two-hour-old action with seeded query samples at meanAfterMs.
func (fx *statsFixture) monitoredAction(t *testing.T, meanAfterMs float64) int64 {
	t.Helper()
	fx.capturePlans(t, time.Now().Add(-3*time.Hour).UTC())
	id, err := fx.run(t, "")
	if err != nil {
		t.Fatalf("ExecuteManual: %v", err)
	}
	// The verifier needs the action's own ANALYZE (not the fixture's) to
	// be visible; on PG14 the collector reports it late, and the monitor
	// then waited out its 3-day cap (CI hang, PR #115).
	plan, err := loadMonitorPlan(fx.ctx, fx.pool, id, r2MonitorConfig())
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	waitBuiltSinceAction(t, fx.ctx, fx.pool, plan)
	time.Sleep(20 * time.Millisecond)
	fx.capturePlans(t, time.Now().UTC())
	acts := fx.actions(t)
	if len(acts) != 1 {
		t.Fatalf("actions = %+v, want one", acts)
	}
	executedAt := time.Now().Add(-2 * time.Hour).UTC()
	seedQuerySamples(t, fx.ctx, fx.pool, fx.qid, executedAt.Add(time.Second), 110, 40,
		meanAfterMs)
	// The row's before_state is JSON; withTarget edits the typed prediction.
	raw, _ := json.Marshal(acts[0].before["predicted_effect"])
	var p verify.Prediction
	if err := json.Unmarshal(raw, &p); err != nil || !p.Predicts() {
		t.Fatalf("recorded prediction %s: %v", raw, err)
	}
	acts[0].before["predicted_effect"] = p
	before := withTarget(t, acts[0].before, fx.qid)
	fx.mustExec(t, `UPDATE sage.action_log SET executed_at = $2, before_state = $3::jsonb
		WHERE id = $1`, id, executedAt, before)
	return id
}

// A mismatched rollback refuses the action before anything runs.
func TestCreateStatisticsRefusesAForeignRollback(t *testing.T) {
	fx := newStatsFixture(t)
	_, err := fx.run(t, "DROP STATISTICS IF EXISTS public.sage_stx_someone_else")
	if !errors.Is(err, ErrRollbackMismatch) {
		t.Fatalf("ExecuteManual = %v, want ErrRollbackMismatch", err)
	}
	if fx.statisticsExist(t) {
		t.Fatal("statistics created although the rollback drops another object")
	}
	if acts := fx.actions(t); len(acts) != 0 {
		t.Fatalf("actions recorded for a refused run: %+v", acts)
	}
}

// PostgreSQL takes SHARE UPDATE EXCLUSIVE on the table for CREATE
// STATISTICS and for ANALYZE, and nothing stronger: the contract's lock.
func TestCreateStatisticsLockIsShareUpdateExclusive(t *testing.T) {
	fx := newStatsFixture(t)
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, sql := range []string{fx.sql, "ANALYZE public." + fx.table} {
		if _, err := tx.Exec(fx.ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows, err := tx.Query(fx.ctx, `SELECT mode FROM pg_locks
		WHERE pid = pg_backend_pid() AND relation = to_regclass($1) AND granted`,
		"public."+fx.table)
	if err != nil {
		t.Fatalf("read locks: %v", err)
	}
	var modes []string
	for rows.Next() {
		var mode string
		if err := rows.Scan(&mode); err != nil {
			t.Fatalf("scan lock: %v", err)
		}
		modes = append(modes, mode)
	}
	rows.Close()
	if len(modes) == 0 {
		t.Fatal("no table lock held")
	}
	for _, mode := range modes {
		switch mode {
		case "AccessShareLock", "ShareUpdateExclusiveLock":
		default:
			t.Fatalf("table lock %s is stronger than SHARE UPDATE EXCLUSIVE (all: %v)",
				mode, modes)
		}
	}
	if !strings.Contains(strings.Join(modes, ","), "ShareUpdateExclusiveLock") {
		t.Fatalf("modes %v lack ShareUpdateExclusiveLock", modes)
	}
}

// Under lock_timeout a busy table fails the whole action: the statistics
// object is not left behind without its ANALYZE.
func TestExecStatisticsIsAtomicUnderLockTimeout(t *testing.T) {
	fx := newStatsFixture(t)
	holder, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(fx.ctx, "LOCK TABLE public."+fx.table+
		" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	err = ExecStatistics(fx.ctx, fx.pool, fx.sql, 10*time.Second, WithLockTimeout(200))
	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("ExecStatistics = %v, want ErrLockNotAvailable", err)
	}
	_ = holder.Rollback(fx.ctx)
	if fx.statisticsExist(t) {
		t.Fatal("statistics object left behind by a failed action")
	}
	if err := ExecStatistics(fx.ctx, fx.pool, fx.sql, 10*time.Second,
		WithLockTimeout(200)); err != nil {
		t.Fatalf("ExecStatistics once the table is free: %v", err)
	}
	if !fx.statisticsExist(t) || !fx.statisticsBuilt(t) {
		t.Fatal("ExecStatistics did not create and build the statistics")
	}
}

// The background path runs the same statement pair for a finding.
func TestRunFindingSQLBuildsStatistics(t *testing.T) {
	fx := newStatsFixture(t)
	f := analyzer.Finding{RecommendedSQL: fx.sql, ObjectIdentifier: "public." + fx.table,
		Detail: map[string]any{}}
	if err := fx.exec.runFindingSQL(fx.ctx, f, ActionPolicyDecision{}); err != nil {
		t.Fatalf("runFindingSQL: %v", err)
	}
	if !fx.statisticsExist(t) || !fx.statisticsBuilt(t) {
		t.Fatal("background run did not create and build the statistics")
	}
}

// backgroundFinding is the fixture's finding as the background cycle
// hands it to processFinding, with the monitors already stopped.
func (fx *statsFixture) backgroundFinding(t *testing.T, rollbackSQL string) analyzer.Finding {
	t.Helper()
	shutdown, cancel := context.WithTimeout(fx.ctx, 10*time.Second)
	defer cancel()
	if err := fx.exec.Shutdown(shutdown); err != nil {
		t.Fatalf("stop monitors: %v", err)
	}
	fx.exec.WithPolicyGate(fixedGate{verdict: policy.VerdictExecute,
		decisionID: insertParkDecision(t, fx.ctx, fx.pool)})
	return analyzer.Finding{Category: "query_create_statistics", Title: "correlated a,b",
		ObjectType: "table", ObjectIdentifier: "public." + fx.table,
		RecommendedSQL: fx.sql, RollbackSQL: rollbackSQL,
		Detail: map[string]any{"queryid": float64(fx.qid)}}
}

// The background path gives the finding its derived rollback before the
// gate, Apply and the monitor see it.
func TestBackgroundStatisticsRecordsItsDerivedRollback(t *testing.T) {
	fx := newStatsFixture(t)
	fx.exec.processFinding(fx.ctx, fx.backgroundFinding(t, ""), false, nil)
	acts := fx.actions(t)
	if len(acts) != 1 {
		t.Fatalf("actions = %+v, want one", acts)
	}
	if want := "DROP STATISTICS IF EXISTS public." + fx.name; acts[0].rollback != want {
		t.Fatalf("recorded rollback = %q, want %q", acts[0].rollback, want)
	}
	if !fx.statisticsExist(t) || !fx.statisticsBuilt(t) {
		t.Fatal("background run did not create and build the statistics")
	}
}

// A finding whose rollback drops another object never runs.
func TestBackgroundStatisticsWithAForeignRollbackDoesNotRun(t *testing.T) {
	fx := newStatsFixture(t)
	fx.exec.processFinding(fx.ctx, fx.backgroundFinding(t,
		"DROP STATISTICS IF EXISTS public.sage_stx_someone_else"), false, nil)
	if acts := fx.actions(t); len(acts) != 0 {
		t.Fatalf("actions = %+v, want none", acts)
	}
	if fx.statisticsExist(t) {
		t.Fatal("statistics created although the rollback drops another object")
	}
}
