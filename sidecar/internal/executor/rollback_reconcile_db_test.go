package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// executor-07: a crash during a rollback left the action in rolling_back
// for ever. The reconciler settles a row older than the DDL timeout by
// probing whether the rollback took effect.

// stuckExecutor is an advisory, enabled executor with the default DDL
// timeout (300 s), so the reconcile grace is six minutes.
func stuckExecutor(t *testing.T, pool *pgxpool.Pool) *Executor {
	t.Helper()
	exec := resumeExecutor(t, pool)
	exec.SetExecutorEnabled(true)
	return exec
}

// insertStuckRollback records an action left in rolling_back, claimed age
// ago (measured_at is set by the claim).
func insertStuckRollback(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, actionType, rollbackSQL string,
	age time.Duration,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome, measured_at)
		VALUES ($1, 'CREATE INDEX CONCURRENTLY rb_stuck ON public.t (a)', $2,
		        'rolling_back', now() - make_interval(secs => $3)) RETURNING id`,
		actionType, rollbackSQL, age.Seconds()).Scan(&id)
	if err != nil {
		t.Fatalf("insert stuck rollback: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	return id
}

// stuckIndex creates a table with one index and returns the index's
// schema-qualified name.
func stuckIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	table, index, create := rbFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, create); err != nil {
		t.Fatalf("create index: %v", err)
	}
	return table, "public." + index
}

func stuckGrace(exec *Executor) time.Duration { return exec.stuckRollbackGrace() }

func reconcileOnce(t *testing.T, ctx context.Context, exec *Executor) int {
	t.Helper()
	n, err := exec.reconcileStuckRollbacks(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return n
}

func TestReconcileStuck_GraceIsDDLTimeoutPlusOneMinute(t *testing.T) {
	exec := New(nil, resumeExecutorConfig(), time.Time{}, nopLog)
	if got := exec.stuckRollbackGrace(); got != 6*time.Minute {
		t.Fatalf("grace = %v, want ddl_timeout (300s) + 1m", got)
	}
	exec.cfg.Safety.DDLTimeoutSeconds = 30
	if got := exec.stuckRollbackGrace(); got != 90*time.Second {
		t.Fatalf("grace = %v, want 30s + 1m", got)
	}
}

func TestReconcileStuck_DroppedIndexBecomesRolledBack(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	gone := fmt.Sprintf("public.rb_gone_%d", time.Now().UnixNano())
	id := insertStuckRollback(t, ctx, pool, "create_index",
		"DROP INDEX CONCURRENTLY IF EXISTS "+gone, stuckGrace(exec)+time.Minute)
	if n := reconcileOnce(t, ctx, exec); n < 1 {
		t.Fatalf("settled %d rows, want the stuck one", n)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back (the index is gone)", outcome)
	}
	if r := actionReason(t, ctx, pool, id); !strings.Contains(r, "interrupted") {
		t.Fatalf("reason = %q, want it to name the interruption", r)
	}
}

func TestReconcileStuck_IndexStillPresentFailsThenRetries(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	_, index := stuckIndex(t, ctx, pool)
	id := insertStuckRollback(t, ctx, pool, "create_index",
		"DROP INDEX CONCURRENTLY IF EXISTS "+index, stuckGrace(exec)+time.Minute)
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rollback_failed" {
		t.Fatalf("outcome = %q, want rollback_failed (the index is still there)", outcome)
	}
	if r := actionReason(t, ctx, pool, id); !strings.Contains(r, "interrupted during rollback") {
		t.Fatalf("reason = %q, want interrupted during rollback", r)
	}
	if indexPresentByName(t, ctx, pool, index) != true {
		t.Fatal("the reconciler must not run DDL")
	}
	// rollback_failed is retryable through the operator path.
	if err := exec.RollbackAction(ctx, id, "operator retry"); err != nil {
		t.Fatalf("retry rollback: %v", err)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome after retry = %q, want rolled_back", outcome)
	}
	if indexPresentByName(t, ctx, pool, index) {
		t.Fatal("the retried rollback did not drop the index")
	}
}

func indexPresentByName(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	index string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, index).
		Scan(&present); err != nil {
		t.Fatalf("index presence: %v", err)
	}
	return present
}

// A drop action's rollback recreates the index: back with the same
// definition is rolled_back, missing is rollback_failed.
func TestReconcileStuck_CreateIndexRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	_, index, create := rbFixture(t, ctx, pool)
	missing := insertStuckRollback(t, ctx, pool, "drop_index", create,
		stuckGrace(exec)+time.Minute)
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, missing); outcome != "rollback_failed" {
		t.Fatalf("missing index: outcome = %q, want rollback_failed", outcome)
	}
	if indexDef(t, ctx, pool, index) != "" {
		t.Fatal("the reconciler must not recreate the index")
	}
	if _, err := pool.Exec(ctx, create); err != nil {
		t.Fatalf("create index: %v", err)
	}
	back := insertStuckRollback(t, ctx, pool, "drop_index", create,
		stuckGrace(exec)+time.Minute)
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, back); outcome != "rolled_back" {
		t.Fatalf("restored index: outcome = %q, want rolled_back", outcome)
	}
}

// A GUC rollback is confirmed against pg_settings: the setting at the
// rollback's value is rolled_back, any other value is rollback_failed.
func TestReconcileStuck_SystemSettingRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	var current string
	if err := pool.QueryRow(ctx, `SELECT setting || COALESCE(unit, '')
		FROM pg_settings WHERE name = 'work_mem'`).Scan(&current); err != nil {
		t.Fatalf("read work_mem: %v", err)
	}
	same := insertStuckRollback(t, ctx, pool, "alter_system",
		"ALTER SYSTEM SET work_mem = '"+current+"'", stuckGrace(exec)+time.Minute)
	other := insertStuckRollback(t, ctx, pool, "alter_system",
		"ALTER SYSTEM SET work_mem = '7777kB'", stuckGrace(exec)+time.Minute)
	if current == "7777kB" {
		t.Fatalf("fixture collision: work_mem is already %s", current)
	}
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, same); outcome != "rolled_back" {
		t.Fatalf("setting at the rollback value: outcome = %q, want rolled_back", outcome)
	}
	if outcome, _ := actionOutcomeFor(t, pool, other); outcome != "rollback_failed" {
		t.Fatalf("setting elsewhere: outcome = %q, want rollback_failed", outcome)
	}
}

// ALTER DATABASE rollbacks read pg_db_role_setting and a fresh session.
func TestReconcileStuck_DatabaseSettingRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	var db string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatalf("current database: %v", err)
	}
	quoted := `"` + strings.ReplaceAll(db, `"`, `""`) + `"`
	reset := insertStuckRollback(t, ctx, pool, "alter_database",
		"ALTER DATABASE "+quoted+" RESET lock_timeout", stuckGrace(exec)+time.Minute)
	set := insertStuckRollback(t, ctx, pool, "alter_database",
		"ALTER DATABASE "+quoted+" SET lock_timeout = '4321ms'", stuckGrace(exec)+time.Minute)
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, reset); outcome != "rolled_back" {
		t.Fatalf("no database setting: outcome = %q, want rolled_back", outcome)
	}
	if outcome, _ := actionOutcomeFor(t, pool, set); outcome != "rollback_failed" {
		t.Fatalf("setting never stored: outcome = %q, want rollback_failed", outcome)
	}
}

// A rollback the reconciler cannot probe is never declared done.
func TestReconcileStuck_UnprobeableRollbackFails(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	id := insertStuckRollback(t, ctx, pool, "analyze", "ANALYZE public.rb_none",
		stuckGrace(exec)+time.Minute)
	empty := insertStuckRollback(t, ctx, pool, "create_index", "",
		stuckGrace(exec)+time.Minute)
	reconcileOnce(t, ctx, exec)
	for _, row := range []int64{id, empty} {
		if outcome, _ := actionOutcomeFor(t, pool, row); outcome != "rollback_failed" {
			t.Fatalf("action %d: outcome = %q, want rollback_failed", row, outcome)
		}
		if r := actionReason(t, ctx, pool, row); !strings.Contains(r, "could not be confirmed") {
			t.Fatalf("action %d: reason = %q, want could not be confirmed", row, r)
		}
	}
}

func TestReconcileStuck_GraceBoundary(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	ddl := exec.cfg.Safety.DDLTimeout()
	gone := fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS public.rb_edge_%d",
		time.Now().UnixNano())
	atTimeout := insertStuckRollback(t, ctx, pool, "create_index", gone, ddl)
	inside := insertStuckRollback(t, ctx, pool, "create_index", gone,
		ddl+59*time.Second)
	past := insertStuckRollback(t, ctx, pool, "create_index", gone,
		ddl+time.Minute+time.Second)
	reconcileOnce(t, ctx, exec)
	for _, id := range []int64{atTimeout, inside} {
		if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolling_back" {
			t.Fatalf("action %d inside the grace: outcome = %q, want untouched", id, outcome)
		}
	}
	if outcome, _ := actionOutcomeFor(t, pool, past); outcome != "rolled_back" {
		t.Fatalf("a minute past the DDL timeout: outcome = %q, want rolled_back", outcome)
	}
}

// Index replacements resume their own rollback; other states are not
// the reconciler's.
func TestReconcileStuck_LeavesOtherRowsAlone(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	old := stuckGrace(exec) + time.Hour
	gone := "DROP INDEX CONCURRENTLY IF EXISTS public.rb_other_none"
	replace := insertStuckRollback(t, ctx, pool, "replace_index", gone, old)
	monitoring := insertStuckRollback(t, ctx, pool, "create_index", gone, old)
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET outcome = 'monitoring'
		WHERE id = $1`, monitoring); err != nil {
		t.Fatalf("set monitoring: %v", err)
	}
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, replace); outcome != "rolling_back" {
		t.Fatalf("replace_index: outcome = %q, want untouched", outcome)
	}
	if outcome, _ := actionOutcomeFor(t, pool, monitoring); outcome != "monitoring" {
		t.Fatalf("monitoring row: outcome = %q, want untouched", outcome)
	}
}

func TestReconcileStuck_ConcurrentReconcilesMakeOneTransition(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	other := stuckExecutor(t, pool)
	id := insertStuckRollback(t, ctx, pool, "create_index",
		"DROP INDEX CONCURRENTLY IF EXISTS public.rb_race_none",
		stuckGrace(exec)+time.Minute)
	var wg sync.WaitGroup
	start := make(chan struct{})
	settled := make([]bool, 4)
	for i := range settled {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			e := exec
			if i%2 == 1 {
				e = other
			}
			ok, err := e.reconcileStuckRollback(ctx, id)
			if err != nil {
				t.Errorf("reconcile %d: %v", i, err)
			}
			settled[i] = ok
		}(i)
	}
	close(start)
	wg.Wait()
	n := 0
	for _, ok := range settled {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d reconciles settled the row, want exactly 1", n)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
}

// A rollback still running in this process (waiting for a DDL slot, for
// example) is not judged until it ends.
func TestReconcileStuck_SkipsWhileWorkInFlight(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	id := insertStuckRollback(t, ctx, pool, "create_index",
		"DROP INDEX CONCURRENTLY IF EXISTS public.rb_inflight_none",
		stuckGrace(exec)+time.Minute)
	_, run, err := exec.inflight.begin(ctx, "rollback in flight")
	if err != nil {
		t.Fatalf("begin in-flight run: %v", err)
	}
	if n := reconcileOnce(t, ctx, exec); n != 0 {
		t.Fatalf("settled %d rows while a run was in flight, want 0", n)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolling_back" {
		t.Fatalf("outcome = %q while in flight, want untouched", outcome)
	}
	exec.inflight.end(run, 0)
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q after the run ended, want rolled_back", outcome)
	}
}

// CHECK-28: after a simulated crash, a fresh executor's first cycle leaves
// no rolling_back row older than the DDL timeout. A replica writes nothing.
func TestReconcileStuck_RunCycleSettlesAfterRestart(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	id := insertStuckRollback(t, ctx, pool, "create_index",
		"DROP INDEX CONCURRENTLY IF EXISTS public.rb_restart_none",
		stuckGrace(exec)+time.Minute)
	stuckExecutor(t, pool).RunCycle(ctx, true)
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolling_back" {
		t.Fatalf("replica cycle: outcome = %q, want untouched", outcome)
	}
	exec.RunCycle(ctx, false)
	exec.monitors.Wait()
	var stuck int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_log
		WHERE outcome = 'rolling_back' AND action_type <> 'replace_index'
		  AND measured_at < now() - make_interval(secs => $1)`,
		exec.cfg.Safety.DDLTimeout().Seconds()).Scan(&stuck); err != nil {
		t.Fatalf("count stuck: %v", err)
	}
	if stuck != 0 {
		t.Fatalf("%d rolling_back rows older than the DDL timeout remain", stuck)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
}
