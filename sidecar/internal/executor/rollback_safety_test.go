package executor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Regression tests for G4-B06, B10, B11, B12, B13, B25, B28, B31 and
// Codex C14/C17: legacy rollback monitoring safety.

type recordedRollback struct {
	mu    sync.Mutex
	sql   []string
	lock  []int
	err   error
	calls int
}

func (r *recordedRollback) exec(
	_ context.Context, sql string, _ time.Duration, opts ...DDLOption,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.sql = append(r.sql, sql)
	r.lock = append(r.lock, applyDDLOpts(opts).lockTimeoutMs)
	return r.err
}

func insertMonitoredAction(
	t *testing.T, pool *pgxpool.Pool, outcome, rollbackSQL, beforeState string,
) int64 {
	t.Helper()
	stored := beforeState
	if isVerificationFixture(beforeState) {
		stored = "{}"
	}
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome, before_state)
		VALUES ('create_index', 'CREATE INDEX CONCURRENTLY rb_probe ON public.t (a)',
		        $1, $2, $3::jsonb) RETURNING id`,
		rollbackSQL, outcome, stored).Scan(&id)
	if err != nil {
		t.Fatalf("insert monitored action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	if isVerificationFixture(beforeState) {
		applyVerificationFixture(t, pool, id, beforeState)
	}
	return id
}

func actionOutcomeFor(t *testing.T, pool *pgxpool.Pool, id int64) (string, bool) {
	t.Helper()
	var outcome string
	var credited bool
	if err := pool.QueryRow(context.Background(), `SELECT outcome,
		toil_minutes_saved IS NOT NULL FROM sage.action_log WHERE id=$1`, id).
		Scan(&outcome, &credited); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	return outcome, credited
}

func allowRollback(context.Context, string) bool { return true }

func monitorConfig(rec *recordedRollback) RollbackMonitorConfig {
	return RollbackMonitorConfig{
		ThresholdPct: 10, WindowMinutes: 0, StatementTimeout: time.Minute,
		LockTimeoutMs: 1500, Authorize: allowRollback, execRollback: rec.exec,
		applyConfig: func(context.Context, string) configApplyOutcome {
			return configApplyOutcome{InEffect: true, Note: "reloaded"}
		},
	}
}

func TestConcurrentIndexRollbackSQL(t *testing.T) {
	tests := map[string]string{
		"CREATE INDEX idx_big ON public.big USING btree (c);": "CREATE INDEX CONCURRENTLY " +
			"idx_big ON public.big USING btree (c);",
		"CREATE UNIQUE INDEX u ON public.big USING btree (c)": "CREATE UNIQUE INDEX " +
			"CONCURRENTLY u ON public.big USING btree (c)",
		"CREATE INDEX CONCURRENTLY idx ON t (a)": "CREATE INDEX CONCURRENTLY idx ON t (a)",
		"DROP INDEX public.idx":                  "DROP INDEX CONCURRENTLY public.idx",
		"ALTER SYSTEM RESET work_mem":            "ALTER SYSTEM RESET work_mem",
	}
	for input, want := range tests {
		if got := concurrentIndexRollbackSQL(input); got != want {
			t.Fatalf("concurrentIndexRollbackSQL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMonitorRollbackIsConcurrentAndLockBounded(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "monitoring",
		"CREATE INDEX idx_big ON public.big USING btree (c);", regressedBeforeState)
	rec := &recordedRollback{}

	MonitorAndRollback(ctx, pool, id, "CREATE INDEX idx_big ON public.big USING btree (c);",
		monitorConfig(rec), nopLog, nil)

	if rec.calls != 1 || !strings.Contains(rec.sql[0], "CONCURRENTLY") {
		t.Fatalf("rollback SQL = %#v, want one concurrent statement", rec.sql)
	}
	if rec.lock[0] != 1500 {
		t.Fatalf("rollback lock_timeout = %dms, want 1500", rec.lock[0])
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
}

func TestMonitorWithoutAuthorizerWithholdsRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "monitoring", "DROP INDEX CONCURRENTLY public.x",
		regressedBeforeState)
	rec := &recordedRollback{}
	cfg := monitorConfig(rec)
	cfg.Authorize = nil

	MonitorAndRollback(ctx, pool, id, "DROP INDEX CONCURRENTLY public.x", cfg, nopLog, nil)

	if rec.calls != 0 {
		t.Fatalf("rollback executed without authorization: %#v", rec.sql)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rollback_skipped" {
		t.Fatalf("outcome = %q, want rollback_skipped", outcome)
	}
}

func TestManualRollbackAuthorizerHonorsExecutorAndTrust(t *testing.T) {
	cfg := wave1PolicyConfig("advisory")
	exec := New(nil, cfg, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	authorize := exec.manualRollbackAuthorizer()
	if !authorize(context.Background(), "DROP INDEX CONCURRENTLY public.x") {
		t.Fatal("enabled advisory executor refused rollback")
	}
	exec.SetExecutorEnabled(false)
	if authorize(context.Background(), "DROP INDEX CONCURRENTLY public.x") {
		t.Fatal("disabled executor authorized rollback")
	}
	exec.SetExecutorEnabled(true)
	if err := exec.SetTrustLevel("observation"); err != nil {
		t.Fatal(err)
	}
	if authorize(context.Background(), "DROP INDEX CONCURRENTLY public.x") {
		t.Fatal("observation trust authorized rollback")
	}
}

func TestMonitorDoesNotOverwriteOperatorRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	for _, before := range []string{`{"cache_hit_ratio": 0.01}`, regressedBeforeState} {
		id := insertMonitoredAction(t, pool, "rolled_back", "DROP INDEX CONCURRENTLY public.x",
			before)
		rec := &recordedRollback{}

		MonitorAndRollback(ctx, pool, id, "DROP INDEX CONCURRENTLY public.x",
			monitorConfig(rec), nopLog, nil)

		outcome, credited := actionOutcomeFor(t, pool, id)
		if outcome != "rolled_back" || credited || rec.calls != 0 {
			t.Fatalf("before=%s outcome=%q credited=%v rollbacks=%d", before, outcome,
				credited, rec.calls)
		}
	}
}

func TestMonitorReloadsConfigAfterAlterSystemRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "monitoring", "ALTER SYSTEM RESET work_mem",
		regressedBeforeState)
	rec := &recordedRollback{}
	cfg := monitorConfig(rec)
	reloaded := ""
	cfg.applyConfig = func(_ context.Context, sql string) configApplyOutcome {
		reloaded = sql
		return configApplyOutcome{InEffect: true, Note: "reloaded"}
	}

	MonitorAndRollback(ctx, pool, id, "ALTER SYSTEM RESET work_mem", cfg, nopLog, nil)

	if reloaded != "ALTER SYSTEM RESET work_mem" {
		t.Fatalf("config not reloaded after GUC rollback (applied %q)", reloaded)
	}
}

func TestMonitorMissingEvidenceIsNotSuccess(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "monitoring", "DROP INDEX CONCURRENTLY public.x", `{}`)
	rec := &recordedRollback{}

	MonitorAndRollback(ctx, pool, id, "DROP INDEX CONCURRENTLY public.x",
		monitorConfig(rec), nopLog, nil)

	outcome, credited := actionOutcomeFor(t, pool, id)
	if outcome == "success" || credited || rec.calls != 0 {
		t.Fatalf("missing evidence produced outcome=%q credited=%v", outcome, credited)
	}
	if outcome != "unverifiable" {
		t.Fatalf("outcome = %q, want unverifiable", outcome)
	}
}

// Replaces TestEvaluateRegressionTriState: the global cache-hit check is
// gone; the judge compares the targeted queries against the frozen
// baseline, and a missing baseline or prediction is never "no regression".
func TestJudgeMonitoredVerdicts(t *testing.T) {
	pool, ctx := requireDB(t)
	cases := map[string]struct {
		before string
		want   string
	}{
		"missing evidence": {`{}`, verify.OutcomeUnverifiable},
		"regressed":        {regressedBeforeState, verify.OutcomeRegressed},
		"improved":         {improvedBeforeState, verify.OutcomeImproved},
		"legacy cache hit": {`{"cache_hit_ratio": 0.01}`, verify.OutcomeUnverifiable},
	}
	cfg := monitorConfig(&recordedRollback{})
	for name, tc := range cases {
		id := insertMonitoredAction(t, pool, "monitoring", "", tc.before)
		plan, err := loadMonitorPlan(ctx, pool, id, cfg)
		if err != nil {
			t.Fatalf("%s: loadMonitorPlan: %v", name, err)
		}
		got := judgeMonitored(ctx, pool, plan, cfg, time.Now())
		if got.Verdict != tc.want {
			t.Fatalf("%s: verdict = %s (%s), want %s", name, got.Verdict, got.Reason, tc.want)
		}
		if got.ActionLogID != id || got.Class != verify.ClassIndexCreate {
			t.Fatalf("%s: outcome identity = %d/%s", name, got.ActionLogID, got.Class)
		}
	}
}

// Replaces TestPerQueryRegressionWithoutDataIsUnverifiable: a target with
// a baseline but no post-action samples is insufficient evidence.
func TestJudgeTargetWithoutSamplesIsInsufficient(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "monitoring", "",
		fixtureBeforeState(verify.ClassIndexCreate, 987654321))
	cfg := monitorConfig(&recordedRollback{})
	plan, err := loadMonitorPlan(ctx, pool, id, cfg)
	if err != nil {
		t.Fatalf("loadMonitorPlan: %v", err)
	}
	if got := judgeMonitored(ctx, pool, plan, cfg, time.Now()); got.Verdict !=
		verify.OutcomeInsufficient {
		t.Fatalf("verdict = %s (%s), want insufficient_evidence", got.Verdict, got.Reason)
	}
}

func TestLegacyHealthQueriesAreScopedToCurrentDatabase(t *testing.T) {
	for name, query := range map[string]string{
		"cache hit": cacheHitRatioSQL, "write latency": writeLatencySQL,
	} {
		if !strings.Contains(query, "current_database()") {
			t.Fatalf("%s query is not scoped to the current database:\n%s", name, query)
		}
	}
}

func TestHysteresisFollowsFindingIdentityAcrossIDs(t *testing.T) {
	pool, ctx := requireDB(t)
	insertFinding := func(status string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
			(category, severity, object_type, object_identifier, title, detail, status)
			VALUES ('hysteresis_probe', 'warning', 'index', 'public.hyst_idx', 'probe',
			        '{}', $1)
			RETURNING id`, status).Scan(&id); err != nil {
			t.Fatalf("insert finding: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "DELETE FROM sage.findings WHERE id=$1", id)
		})
		return id
	}
	oldID := insertFinding("resolved")
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, finding_id, sql_executed, outcome)
		VALUES ('drop_index', $1, 'DROP INDEX CONCURRENTLY public.hyst_idx', 'rolled_back')
		RETURNING id`, oldID).Scan(&actionID); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", actionID)
	})
	newID := insertFinding("open")

	if !CheckHysteresis(ctx, pool, newID, 7) {
		t.Fatal("re-inserted finding escaped rollback cooldown")
	}
}

func TestResumeOrphanedMonitorsFinishesInterruptedActions(t *testing.T) {
	pool, ctx := requireDB(t)
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Trust.RollbackWindowMinutes = 1
	exec := New(pool, cfg, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	// The old fixture ({"cache_hit_ratio": 0.01}) was credited "success"
	// with no evidence about the action; resumed monitors now need the
	// targeted queries to have improved (Phase 1.3).
	id := insertMonitoredAction(t, pool, "interrupted", "DROP INDEX CONCURRENTLY public.x",
		improvedBeforeState)
	unproven := insertMonitoredAction(t, pool, "interrupted",
		"DROP INDEX CONCURRENTLY public.y", `{"cache_hit_ratio": 0.01}`)
	for _, aged := range []int64{id, unproven} {
		if _, err := pool.Exec(ctx, `UPDATE sage.action_log
			SET executed_at = now() - interval '1 hour' WHERE id=$1`, aged); err != nil {
			t.Fatalf("age action: %v", err)
		}
	}

	if err := exec.resumeOrphanedMonitors(ctx); err != nil {
		t.Fatalf("resumeOrphanedMonitors: %v", err)
	}
	exec.monitors.Wait()

	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "success" {
		t.Fatalf("resumed outcome = %q, want success", outcome)
	}
	if outcome, _ := actionOutcomeFor(t, pool, unproven); outcome != "unverifiable" {
		t.Fatalf("resumed outcome without evidence = %q, want unverifiable", outcome)
	}
}

// alterDatabaseProbeSQL builds an ALTER DATABASE statement for the package
// fixture database (an autonomous-eligible transactional statement) and
// resets the setting after the test.
func alterDatabaseProbeSQL(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, clause string,
) string {
	t.Helper()
	var database string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"ALTER DATABASE "+database+" RESET work_mem")
	})
	return "ALTER DATABASE " + database + " " + clause
}
