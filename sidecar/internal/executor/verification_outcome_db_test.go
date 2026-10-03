package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Phase 1.3 integration tests against real Postgres: every action class
// is judged against what it targeted, and the verdict lands in
// sage.action_outcome with predicted vs observed.

type verifiedActionRow struct {
	sql, rollback, before string
	executedAt            time.Time
	decisionID            int64
}

func insertVerifiedAction(t *testing.T, pool *pgxpool.Pool, row verifiedActionRow) int64 {
	t.Helper()
	var id int64
	var decision any
	if row.decisionID > 0 {
		decision = row.decisionID
	}
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome, before_state, executed_at,
		 decision_id)
		VALUES ($1, $2, NULLIF($3, ''), 'monitoring', $4::jsonb, $5, $6) RETURNING id`,
		categorizeAction(row.sql), row.sql, row.rollback, row.before, row.executedAt,
		decision).Scan(&id)
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "UPDATE sage.action_log SET verification_id=NULL WHERE id=$1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.verification WHERE action_log_id=$1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	return id
}

func storedOutcome(t *testing.T, pool *pgxpool.Pool, id int64) verify.Outcome {
	t.Helper()
	got, err := verify.NewOutcomeStore(pool).Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read outcome for action %d: %v", id, err)
	}
	return got
}

func verificationVerdict(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var verdict *string
	if err := pool.QueryRow(context.Background(), `SELECT v.verdict FROM sage.action_log al
		LEFT JOIN sage.verification v ON v.id = al.verification_id WHERE al.id=$1`, id).
		Scan(&verdict); err != nil {
		t.Fatalf("read verification: %v", err)
	}
	if verdict == nil {
		return ""
	}
	return *verdict
}

// beforeStateJSON builds a before_state with a prediction, targets and a
// frozen baseline (600 calls at baselineMs per target).
func beforeStateJSON(p verify.Prediction, baselineMs float64, extra map[string]any) string {
	state := map[string]any{"predicted_effect": p, "target_queryids": p.TargetQueryIDs}
	baseline := map[string]any{}
	for _, qid := range p.TargetQueryIDs {
		baseline[strconv.FormatInt(qid, 10)] = verify.Measurement{Samples: 600,
			AverageLatency: time.Duration(baselineMs * float64(time.Millisecond)),
			StdErr:         50 * time.Microsecond, Buckets: 30}
	}
	state["verify_baseline"] = baseline
	for k, v := range extra {
		state[k] = v
	}
	raw, _ := json.Marshal(state)
	return string(raw)
}

func dropFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	table := fmt.Sprintf("vo_drop_%d", time.Now().UnixNano())
	index := table + "_a"
	for _, sql := range []string{"CREATE TABLE public." + table + " (a int)",
		"CREATE INDEX " + index + " ON public." + table + " (a)",
		"DROP INDEX public." + index} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	return table, index
}

func dropMonitorConfig(window time.Duration) RollbackMonitorConfig {
	return RollbackMonitorConfig{ThresholdPct: 10, WindowMinutes: 15, CapMinutes: 4320,
		DropWindow: window, StatementTimeout: time.Minute, LockTimeoutMs: 1500,
		Authorize: allowRollback}
}

func dropAction(t *testing.T, pool *pgxpool.Pool, table, index string, qid int64,
	executedAt time.Time) (int64, string) {
	t.Helper()
	rollback := "CREATE INDEX " + index + " ON public." + table + " USING btree (a)"
	p := rulePrediction(0)
	p.Class, p.Metric, p.TargetQueryIDs = verify.ClassIndexDrop,
		verify.MetricMeanExecTime, []int64{qid}
	id := insertVerifiedAction(t, pool, verifiedActionRow{
		sql: "DROP INDEX CONCURRENTLY public." + index, rollback: rollback,
		before: beforeStateJSON(p, fixtureBaselineMs, nil), executedAt: executedAt})
	return id, rollback
}

// createFixture is an applied CREATE INDEX with a hypopg prediction of
// -40% for one target, executed two hours ago.
type createFixture struct {
	exec       *Executor
	id, qid    int64
	table      string
	index      string
	executedAt time.Time
}

func newCreateFixture(t *testing.T) (createFixture, context.Context) {
	t.Helper()
	table, exec, ctx := verifiedTable(t, "public")
	f := createFixture{exec: exec, table: table, index: "public." + table + "_vo",
		executedAt: time.Now().Add(-2 * time.Hour).UTC(),
		qid:        int64(8_700_000_000) + time.Now().UnixNano()%1_000_000}
	if _, err := exec.pool.Exec(ctx, "CREATE INDEX "+table+"_vo ON public."+table+
		" (a)"); err != nil {
		t.Fatalf("create index: %v", err)
	}
	change := -40.0
	p := verify.Prediction{Class: verify.ClassIndexCreate, Method: verify.MethodHypoPG,
		Metric: verify.MetricMeanExecTime, TargetQueryIDs: []int64{f.qid},
		ExpectedChangePct: &change}
	before := map[string]any{"predicted_effect": p, "target_queryids": []int64{f.qid}}
	exec.recordCreatedIndexIdentity(ctx, f.index, before)
	raw, _ := json.Marshal(before)
	f.id = insertVerifiedAction(t, exec.pool, verifiedActionRow{
		sql:      "CREATE INDEX CONCURRENTLY " + table + "_vo ON public." + table + " (a)",
		rollback: "DROP INDEX CONCURRENTLY IF EXISTS " + f.index, before: string(raw),
		executedAt: f.executedAt, decisionID: insertParkDecision(t, ctx, exec.pool)})
	return f, ctx
}

// An index create whose targeted queries got faster is improved, with
// predicted vs observed recorded, the change kept and credited.
func TestOutcome_IndexCreateImprovingTargetsIsImproved(t *testing.T) {
	f, ctx := newCreateFixture(t)
	pool := f.exec.pool
	seedQuerySamples(t, ctx, pool, f.qid, f.executedAt.Add(-2*time.Hour), 119, 10, 20)
	seedContinuation(t, ctx, pool, f.qid, f.executedAt, 119, 10, 8)

	f.exec.indexVerification.now = func() time.Time { return f.executedAt }
	err := f.exec.indexVerification.WatchApplied(ctx, verifiedIndexAction{
		WatchID: fmt.Sprintf("index-action-%d", f.id), Table: "public." + f.table,
		IndexName: f.index, QueryIDs: []int64{f.qid},
		Criterion:   verify.Criterion{Kind: "per_query_latency"},
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS " + f.index}, f.id)
	if err != nil {
		t.Fatalf("WatchApplied: %v", err)
	}

	if outcome, _ := actionOutcomeFor(t, pool, f.id); outcome != "success" {
		t.Fatalf("action outcome = %q, want success", outcome)
	}
	got := storedOutcome(t, pool, f.id)
	if got.Verdict != verify.OutcomeImproved || got.Tolerance != verify.ToleranceMet {
		t.Fatalf("outcome = %s/%s (%s), want improved/met", got.Verdict, got.Tolerance,
			got.Reason)
	}
	if got.Observed.ChangePct == nil || *got.Observed.ChangePct > -55 ||
		*got.Observed.ChangePct < -65 {
		t.Fatalf("observed change = %v, want about -60%% (20 -> 8 ms)",
			got.Observed.ChangePct)
	}
	if got.Predicted.Method != verify.MethodHypoPG || *got.Predicted.ExpectedChangePct != -40 {
		t.Fatalf("predicted = %+v, want the hypopg -40%%", got.Predicted)
	}
	if v := verificationVerdict(t, pool, f.id); v != "success" {
		t.Fatalf("verification verdict = %q, want success", v)
	}
}

// seedContinuation appends samples to an existing cumulative series so
// the before and after windows share one counter history.
func seedContinuation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	start time.Time, minutes int, callsPerMinute int64, meanMs float64) {
	t.Helper()
	var calls int64
	var total float64
	if err := pool.QueryRow(ctx, `SELECT calls, total_exec_time FROM sage.query_store
		WHERE queryid=$1 ORDER BY captured_at DESC LIMIT 1`, qid).
		Scan(&calls, &total); err != nil {
		t.Fatalf("read series tail: %v", err)
	}
	for i := 1; i <= minutes; i++ {
		calls += callsPerMinute
		total += float64(callsPerMinute) * meanMs * (1 + 0.02*float64(i%3-1))
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES ($1, $2, $3, $4, $5)`, start.Add(time.Duration(i)*time.Minute), qid,
			calls, total, total/float64(calls)); err != nil {
			t.Fatalf("seed continuation: %v", err)
		}
	}
}

// A drop whose targeted query regresses is regressed and soft-dropped
// back: the kept definition is re-created at the first check, long before
// the 7-day business cycle ends.
func TestOutcome_DropRegressingTargetIsRecreated(t *testing.T) {
	pool, ctx := requireDB(t)
	table, index := dropFixture(t, ctx, pool)
	executedAt := time.Now().Add(-2 * time.Hour).UTC()
	qid := int64(8_710_000_000) + time.Now().UnixNano()%1_000_000
	id, rollback := dropAction(t, pool, table, index, qid, executedAt)
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 110, 40,
		fixtureBaselineMs*3)

	MonitorAndRollback(ctx, pool, id, rollback, dropMonitorConfig(168*time.Hour), nopLog,
		nil)

	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("action outcome = %q (%s), want rolled_back", outcome,
			actionReason(t, ctx, pool, id))
	}
	if def := indexDef(t, ctx, pool, index); !strings.Contains(def, "(a)") {
		t.Fatalf("soft-dropped index was not re-created: %q", def)
	}
	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeRegressed || got.Tolerance != verify.ToleranceMissed {
		t.Fatalf("outcome = %s/%s, want regressed/missed", got.Verdict, got.Tolerance)
	}
	soft, _ := got.Evidence["soft_drop"].(map[string]any)
	if soft["trigger"] != "query_regression" || soft["definition"] != rollback {
		t.Fatalf("soft-drop evidence = %+v", got.Evidence)
	}
	if got.WindowEnd == nil || got.WindowEnd.Sub(executedAt) > 24*time.Hour {
		t.Fatalf("decided at %v: the first miss must not wait for the business cycle",
			got.WindowEnd)
	}
}

// Soft-drop: an active hint that names the dropped index is a plan miss
// even before any query regresses.
func TestOutcome_SoftDropRecreatesOnHintReference(t *testing.T) {
	pool, ctx := requireDB(t)
	table, index := dropFixture(t, ctx, pool)
	executedAt := time.Now().Add(-2 * time.Hour).UTC()
	qid := int64(8_720_000_000) + time.Now().UnixNano()%1_000_000
	id, rollback := dropAction(t, pool, table, index, qid, executedAt)
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 110, 40,
		fixtureBaselineMs)
	var hintID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status) VALUES ($1, $2, 'test', 'active')
		RETURNING id`, qid, "IndexScan("+table+" "+index+")").Scan(&hintID); err != nil {
		t.Fatalf("insert hint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.query_hints WHERE id=$1",
			hintID)
	})

	MonitorAndRollback(ctx, pool, id, rollback, dropMonitorConfig(168*time.Hour), nopLog,
		nil)

	if def := indexDef(t, ctx, pool, index); def == "" {
		t.Fatal("index referenced by an active hint was not re-created")
	}
	got := storedOutcome(t, pool, id)
	soft, _ := got.Evidence["soft_drop"].(map[string]any)
	if got.Verdict != verify.OutcomeRegressed || soft["trigger"] != "hint_reference" {
		t.Fatalf("outcome = %s evidence = %+v, want regressed by hint_reference",
			got.Verdict, got.Evidence)
	}
}

// A drop whose reads held through the whole business cycle is improved:
// its predicted effect (reads unchanged, index gone) was observed.
func TestOutcome_DropHeldThroughBusinessCycleIsImproved(t *testing.T) {
	pool, ctx := requireDB(t)
	table, index := dropFixture(t, ctx, pool)
	executedAt := time.Now().Add(-3 * time.Hour).UTC()
	qid := int64(8_730_000_000) + time.Now().UnixNano()%1_000_000
	id, rollback := dropAction(t, pool, table, index, qid, executedAt)
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 170, 40,
		fixtureBaselineMs)

	MonitorAndRollback(ctx, pool, id, rollback, dropMonitorConfig(2*time.Hour), nopLog, nil)

	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "success" {
		t.Fatalf("action outcome = %q (%s), want success", outcome,
			actionReason(t, ctx, pool, id))
	}
	if def := indexDef(t, ctx, pool, index); def != "" {
		t.Fatalf("a drop that held was re-created: %s", def)
	}
	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeImproved || got.Tolerance != verify.ToleranceMet {
		t.Fatalf("outcome = %s/%s (%s), want improved/met", got.Verdict, got.Tolerance,
			got.Reason)
	}
}

// Before the business cycle ends a healthy drop is still being watched:
// nothing is decided and nothing is credited.
func TestOutcome_DropIsNotDecidedBeforeBusinessCycle(t *testing.T) {
	pool, _ := requireDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	table, index := dropFixture(t, ctx, pool)
	executedAt := time.Now().Add(-2 * time.Hour).UTC()
	qid := int64(8_740_000_000) + time.Now().UnixNano()%1_000_000
	id, rollback := dropAction(t, pool, table, index, qid, executedAt)
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 110, 40,
		fixtureBaselineMs)

	MonitorAndRollback(ctx, pool, id, rollback, dropMonitorConfig(168*time.Hour), nopLog,
		nil)

	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "monitoring" {
		t.Fatalf("action outcome = %q, want still monitoring inside the cycle", outcome)
	}
	if o, err := verify.NewOutcomeStore(pool).Get(context.Background(), id); err == nil &&
		o.Verdict != verify.OutcomePending {
		t.Fatalf("outcome decided early: %+v", o)
	}
}

// A GUC change whose targeted metric did not move is neutral: kept, not
// credited, not rolled back.
func TestOutcome_GUCWithoutMeasurableEffectIsNeutral(t *testing.T) {
	pool, ctx := requireDB(t)
	// The workload spilled before the change (a fresh test database has no
	// temp files, which is "nothing to improve", not "no effect"): the
	// pre-change rate predicts exactly the spills seen since, so the
	// change made no measurable difference.
	start := forceTempSpill(t, ctx, pool)
	current := start
	for i := 0; i < 20 && current-start < 4; i++ {
		current = forceTempSpill(t, ctx, pool)
	}
	if current-start < 3 {
		t.Fatalf("could not produce enough temp-file spills (%v)", current-start)
	}
	at := time.Now().Add(-10 * time.Minute).UTC()
	baseline := outcomeBaseline{Metric: metricTempSpills, At: at,
		Counters: map[string]float64{"temp_files": start,
			"rate_per_sec": (current - start) / 600}}
	p := configPrediction(metricTempSpills)
	p.Class = verify.ClassGUC
	raw, _ := json.Marshal(map[string]any{"predicted_effect": p,
		"config_change": map[string]any{"kind": "guc", "name": "work_mem",
			"outcome": baseline}})
	id := insertVerifiedAction(t, pool, verifiedActionRow{
		sql: "ALTER SYSTEM SET work_mem = '64MB'", rollback: "ALTER SYSTEM RESET work_mem",
		before: string(raw), executedAt: at, decisionID: insertParkDecision(t, ctx, pool)})
	rec := &recordedRollback{}

	MonitorAndRollback(ctx, pool, id, "ALTER SYSTEM RESET work_mem", monitorConfig(rec),
		nopLog, nil)

	outcome, credited := actionOutcomeFor(t, pool, id)
	if outcome != "success" || credited || rec.calls != 0 {
		t.Fatalf("outcome = %q (%s) credited=%v rollbacks=%d, want kept, uncredited", outcome,
			actionReason(t, ctx, pool, id),
			credited, rec.calls)
	}
	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeNeutral || got.Tolerance != verify.ToleranceMissed ||
		got.Observed.Metric != metricTempSpills {
		t.Fatalf("outcome = %+v, want neutral/missed on temp_spills", got)
	}
	if v := verificationVerdict(t, pool, id); v != "unverifiable" {
		t.Fatalf("verification verdict = %q, want unverifiable (never success)", v)
	}
}

// Too few calls after the change is insufficient evidence at the cap:
// recorded as such, never as success, never credited.
func TestOutcome_TooFewCallsIsInsufficientEvidence(t *testing.T) {
	pool, ctx := requireDB(t)
	executedAt := time.Now().Add(-30 * time.Minute).UTC()
	qid := int64(8_750_000_000) + time.Now().UnixNano()%1_000_000
	p := rulePrediction(-30)
	p.Class, p.Metric, p.TargetQueryIDs = verify.ClassIndexCreate,
		verify.MetricMeanExecTime, []int64{qid}
	id := insertVerifiedAction(t, pool, verifiedActionRow{
		sql:      "CREATE INDEX CONCURRENTLY vo_few ON public.t (a)",
		rollback: "DROP INDEX CONCURRENTLY IF EXISTS public.vo_few",
		before:   beforeStateJSON(p, fixtureBaselineMs, nil), executedAt: executedAt,
		decisionID: insertParkDecision(t, ctx, pool)})
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 5, 1, 5)
	rec := &recordedRollback{}

	MonitorAndRollback(ctx, pool, id, "DROP INDEX CONCURRENTLY IF EXISTS public.vo_few",
		monitorConfig(rec), nopLog, nil)

	outcome, credited := actionOutcomeFor(t, pool, id)
	if outcome == "success" || credited || rec.calls != 0 {
		t.Fatalf("too few calls produced outcome=%q credited=%v rollbacks=%d", outcome,
			credited, rec.calls)
	}
	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeInsufficient || got.Tolerance != verify.ToleranceUnmeasured {
		t.Fatalf("outcome = %s/%s, want insufficient_evidence/unmeasured", got.Verdict,
			got.Tolerance)
	}
	if v := verificationVerdict(t, pool, id); v != "unverifiable" {
		t.Fatalf("verification verdict = %q, want unverifiable", v)
	}
}

// VACUUM is verified by the dead tuples it was predicted to remove, not
// marked successful the moment it returns.
func TestOutcome_VacuumVerifiedByDeadTuples(t *testing.T) {
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("vo_vac_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+table+
		" AS SELECT g AS a FROM generate_series(1, 4000) g"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	deleteAndFlush(t, ctx, pool, "DELETE FROM public."+table+" WHERE a > 1000")
	waitForDeadTuples(t, ctx, pool, table)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	sql := "VACUUM public." + table
	before := map[string]any{}
	p := exec.predictAction(ctx, sql, nil, before)
	if p.Baseline == nil || *p.Baseline < 1000 {
		t.Fatalf("vacuum prediction baseline = %v, want the dead tuples", p.Baseline)
	}
	raw, _ := json.Marshal(before)
	id := insertVerifiedAction(t, pool, verifiedActionRow{sql: sql, before: string(raw),
		executedAt: time.Now().UTC()})
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	exec.verifyImmediate(ctx, id)

	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeImproved || got.Observed.ChangePct == nil ||
		*got.Observed.ChangePct > -50 {
		t.Fatalf("vacuum outcome = %+v, want improved with dead tuples removed", got)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "success" {
		t.Fatalf("action outcome = %q, want success", outcome)
	}
}

func deleteAndFlush(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	// PG15+ flushes pending stats on demand; older servers flush within
	// a second of the transaction ending.
	_, _ = conn.Exec(ctx, "SELECT pg_stat_force_next_flush()")
	_, _ = conn.Exec(ctx, "SELECT 1")
}

func waitForDeadTuples(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var dead int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(n_dead_tup, 0) FROM
			pg_stat_user_tables WHERE relid = to_regclass($1)`, "public."+table).
			Scan(&dead); err == nil && dead >= 1000 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dead tuples on %s never reached the statistics", table)
}

// VACUUM (FREEZE) is judged by the relfrozenxid age it was run to
// advance, not by dead tuples (a freshly loaded table has none).
func TestOutcome_VacuumFreezeVerifiedByXIDAge(t *testing.T) {
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("vo_frz_%d", time.Now().UnixNano())
	for _, sql := range []string{"CREATE TABLE public." + table + " (a int)",
		"INSERT INTO public." + table + " SELECT generate_series(1, 100)",
		"SELECT txid_current()"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	sql := "VACUUM (FREEZE) public." + table
	before := map[string]any{}
	p := exec.predictAction(ctx, sql, nil, before)
	if p.Metric != verify.MetricFrozenXIDAge || p.Baseline == nil || *p.Baseline < 1 {
		t.Fatalf("freeze prediction = %+v, want a relfrozenxid age baseline", p)
	}
	raw, _ := json.Marshal(before)
	id := insertVerifiedAction(t, pool, verifiedActionRow{sql: sql, before: string(raw),
		executedAt: time.Now().UTC()})
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("vacuum freeze: %v", err)
	}

	exec.verifyImmediate(ctx, id)

	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeImproved || got.Observed.Metric != verify.MetricFrozenXIDAge {
		t.Fatalf("freeze outcome = %+v, want improved on relfrozenxid age", got)
	}
}
