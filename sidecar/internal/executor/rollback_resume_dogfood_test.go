package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Dogfood lifeos-1 finding 9a: at startup v1.8.0 resumed monitors for
// actions from June (3.5 months old), compared their June before_state
// with today's metrics, called that a regression and ran rollback DDL.
// A monitor whose window ended long ago is not checked: the action is
// marked verification_expired with why. Finding 9b: a rollback whose
// target index already exists with the same definition is
// already_restored, not rollback_failed.

// rbFixture creates a table and returns its schema-qualified name and a
// rollback that would recreate an index on it.
func rbFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string, string) {
	t.Helper()
	table := fmt.Sprintf("rb_dogfood_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+table+" (a int, b int)"); err != nil {
		t.Fatalf("table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	index := table + "_ab"
	rollback := "CREATE INDEX " + index + " ON public." + table + " USING btree (a, b)"
	return table, index, rollback
}

func indexDef(t *testing.T, ctx context.Context, pool *pgxpool.Pool, index string) string {
	t.Helper()
	var def *string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass($1))`,
		"public."+index).Scan(&def); err != nil {
		t.Fatalf("indexdef: %v", err)
	}
	if def == nil {
		return ""
	}
	return *def
}

func resumeExecutor(t *testing.T, pool *pgxpool.Pool) *Executor {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Trust.RollbackWindowMinutes = 15
	exec := New(pool, cfg, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	return exec
}

func ageAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64,
	age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log
		SET executed_at = now() - make_interval(secs => $2) WHERE id = $1`, id,
		age.Seconds()); err != nil {
		t.Fatalf("age action: %v", err)
	}
}

func actionReason(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var reason *string
	if err := pool.QueryRow(ctx, `SELECT rollback_reason FROM sage.action_log
		WHERE id = $1`, id).Scan(&reason); err != nil {
		t.Fatalf("reason: %v", err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

func TestResume_ExpiredMonitorIsNotCheckedOrRolledBack(t *testing.T) {
	pool, ctx := requireDB(t)
	_, index, rollback := rbFixture(t, ctx, pool)
	id := insertMonitoredAction(t, pool, "interrupted", rollback, regressedBeforeState)
	ageAction(t, ctx, pool, id, 112*24*time.Hour)
	exec := resumeExecutor(t, pool)
	if err := exec.resumeOrphanedMonitors(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	exec.monitors.Wait()
	outcome, credited := actionOutcomeFor(t, pool, id)
	if outcome != "verification_expired" || credited {
		t.Fatalf("outcome = %q (credited %v), want verification_expired", outcome, credited)
	}
	if r := actionReason(t, ctx, pool, id); !strings.Contains(r, "window ended") ||
		!strings.Contains(r, "baseline") {
		t.Fatalf("reason = %q, want why it was not checked", r)
	}
	if def := indexDef(t, ctx, pool, index); def != "" {
		t.Fatalf("rollback DDL ran for an expired monitor: %s", def)
	}
	// Idempotent: a second resume finds nothing to do.
	if err := exec.resumeOrphanedMonitors(ctx); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	exec.monitors.Wait()
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "verification_expired" {
		t.Fatalf("second resume changed the outcome to %q", outcome)
	}
}

// Inside the horizon (a restart shortly after the window ended) the
// monitor still checks once, immediately, and rolls back a regression.
func TestResume_WithinHorizonStillChecksAndRollsBack(t *testing.T) {
	pool, ctx := requireDB(t)
	_, index, rollback := rbFixture(t, ctx, pool)
	id := insertMonitoredAction(t, pool, "interrupted", rollback, regressedBeforeState)
	ageAction(t, ctx, pool, id, 40*time.Minute)
	exec := resumeExecutor(t, pool)
	if err := exec.resumeOrphanedMonitors(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	exec.monitors.Wait()
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	if def := indexDef(t, ctx, pool, index); def == "" {
		t.Fatal("rollback did not recreate the index")
	}
}

func TestVerificationExpired_Boundaries(t *testing.T) {
	now := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	window := 15 * time.Minute
	cases := []struct {
		age     time.Duration
		window  time.Duration
		expired bool
	}{
		{10 * time.Minute, window, false},
		{75 * time.Minute, window, false}, // window + 1 h grace: still checked
		{75*time.Minute + time.Second, window, true},
		{112 * 24 * time.Hour, window, true},
		{5 * time.Hour, 3 * time.Hour, false}, // a long window is its own grace
		{6*time.Hour + time.Second, 3 * time.Hour, true},
		{0, 0, false},
	}
	for _, c := range cases {
		if got := verificationExpired(now.Add(-c.age), c.window, now); got != c.expired {
			t.Errorf("age %s window %s: expired = %v, want %v", c.age, c.window, got,
				c.expired)
		}
	}
}

// 9b: the index the rollback would create already exists with the same
// definition (the application recreated it): already_restored, no DDL.
func TestRollback_AlreadyRestoredIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	_, index, rollback := rbFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, rollback); err != nil {
		t.Fatalf("app recreates the index: %v", err)
	}
	id := insertMonitoredAction(t, pool, "monitoring", rollback+";", regressedBeforeState)
	MonitorAndRollback(ctx, pool, id, rollback+";", RollbackMonitorConfig{ThresholdPct: 10,
		StatementTimeout: time.Minute, LockTimeoutMs: 1500, Authorize: allowRollback},
		nopLog, nil)
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "already_restored" {
		t.Fatalf("outcome = %q (%s), want already_restored", outcome,
			actionReason(t, ctx, pool, id))
	}
	if def := indexDef(t, ctx, pool, index); !strings.Contains(def, "(a, b)") {
		t.Fatalf("index changed: %q", def)
	}
}

// A same-named index with another definition is not "restored": the
// rollback fails, naming the conflict, and the index is left alone.
func TestRollback_ConflictingIndexIsAFailure(t *testing.T) {
	pool, ctx := requireDB(t)
	table, index, rollback := rbFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, "CREATE INDEX "+index+" ON public."+table+" (b)"); err != nil {
		t.Fatalf("conflicting index: %v", err)
	}
	id := insertMonitoredAction(t, pool, "monitoring", rollback, regressedBeforeState)
	MonitorAndRollback(ctx, pool, id, rollback, RollbackMonitorConfig{ThresholdPct: 10,
		StatementTimeout: time.Minute, LockTimeoutMs: 1500, Authorize: allowRollback},
		nopLog, nil)
	outcome, _ := actionOutcomeFor(t, pool, id)
	if r := actionReason(t, ctx, pool, id); outcome != "rollback_failed" ||
		!strings.Contains(r, "different definition") {
		t.Fatalf("outcome = %q (%s), want rollback_failed naming the conflict", outcome, r)
	}
	if def := indexDef(t, ctx, pool, index); !strings.Contains(def, "(b)") {
		t.Fatalf("conflicting index replaced: %q", def)
	}
}

func TestSameIndexDefinition(t *testing.T) {
	def := "CREATE INDEX idx_x ON public.t USING btree (a, b)"
	same := []string{def, def + ";", "create index concurrently idx_x on public.t " +
		"using btree (a, b);", "CREATE INDEX IF NOT EXISTS idx_x ON public.t USING btree " +
		"(a,  b)", "  CREATE   INDEX idx_x\nON public.t USING btree (a, b)  "}
	for _, s := range same {
		if !sameIndexDefinition(def, s) {
			t.Errorf("%q should match %q", s, def)
		}
	}
	for _, s := range []string{"CREATE INDEX idx_x ON public.t USING btree (b, a)",
		"CREATE UNIQUE INDEX idx_x ON public.t USING btree (a, b)",
		"CREATE INDEX idx_x ON public.t USING hash (a)", ""} {
		if sameIndexDefinition(def, s) {
			t.Errorf("%q should not match %q", s, def)
		}
	}
}

// Post-test audit (mutation survived): the already-restored check runs
// before any DDL, so no failing CREATE INDEX is even attempted.
func TestRollback_AlreadyRestoredRunsNoDDL(t *testing.T) {
	pool, ctx := requireDB(t)
	_, _, rollback := rbFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, rollback); err != nil {
		t.Fatalf("app recreates the index: %v", err)
	}
	id := insertMonitoredAction(t, pool, "monitoring", rollback, regressedBeforeState)
	rec := &recordedRollback{}
	MonitorAndRollback(ctx, pool, id, rollback, monitorConfig(rec), nopLog, nil)
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "already_restored" ||
		rec.calls != 0 {
		t.Fatalf("outcome = %q with %d DDL calls, want already_restored and none",
			outcome, rec.calls)
	}
}
