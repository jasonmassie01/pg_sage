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

// executor-07 gap 3: a failed terminal write after a successful rollback
// left the row in rolling_back. Writes now retry, then log and return the
// error; the reconciler settles what still failed.

// injectOutcomeFault makes the first failures UPDATEs of action id to
// outcome fail. A sequence counts the attempts: nextval is not rolled back
// with the failed statement.
func injectOutcomeFault(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, outcome string,
	failures int,
) func() {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	seq, fn, trg := "public.rb_fault_seq_"+suffix, "public.rb_fault_fn_"+suffix,
		"rb_fault_trg_"+suffix
	stmts := []string{
		"CREATE SEQUENCE " + seq,
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $f$
			BEGIN
			  IF NEW.outcome = %s AND nextval('%s') <= %d THEN
			    RAISE EXCEPTION 'injected terminal write failure';
			  END IF;
			  RETURN NEW;
			END $f$`, fn, quoteTestLiteral(outcome), seq, failures),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON sage.action_log FOR EACH ROW
			WHEN (NEW.id = %d) EXECUTE FUNCTION %s()`, trg, id, fn),
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("inject fault: %v", err)
		}
	}
	remove := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DROP TRIGGER IF EXISTS "+trg+" ON sage.action_log")
		_, _ = pool.Exec(bg, "DROP FUNCTION IF EXISTS "+fn+"()")
		_, _ = pool.Exec(bg, "DROP SEQUENCE IF EXISTS "+seq)
	}
	t.Cleanup(remove)
	return remove
}

func quoteTestLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

type outcomeLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *outcomeLog) log(_ string, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *outcomeLog) contains(parts ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range c.lines {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return true
		}
	}
	return false
}

func TestRecordActionOutcome_RetriesATransientFailure(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "rolling_back", "", "{}")
	injectOutcomeFault(t, ctx, pool, id, "rolled_back", 1)
	logs := &outcomeLog{}
	if err := recordActionOutcome(ctx, pool, logs.log, id, "rolled_back",
		"rollback done"); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back after the retry", outcome)
	}
	if r := actionReason(t, ctx, pool, id); r != "rollback done" {
		t.Fatalf("reason = %q, want the recorded reason", r)
	}
}

func TestRecordActionOutcome_PersistentFailureIsReturnedAndLogged(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "rolling_back", "", "{}")
	injectOutcomeFault(t, ctx, pool, id, "rolled_back", 1000)
	logs := &outcomeLog{}
	err := recordActionOutcome(ctx, pool, logs.log, id, "rolled_back", "rollback done")
	if err == nil {
		t.Fatal("a write that never succeeds must return its error")
	}
	if !strings.Contains(err.Error(), "injected terminal write failure") ||
		!strings.Contains(err.Error(), fmt.Sprint(id)) {
		t.Fatalf("error = %v, want the cause and the action id", err)
	}
	if !logs.contains(fmt.Sprint(id), "rolled_back") {
		t.Fatalf("log = %v, want the action id and outcome", logs.lines)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolling_back" {
		t.Fatalf("outcome = %q, want unchanged", outcome)
	}
}

func TestRecordActionOutcome_CancelledContextStopsRetrying(t *testing.T) {
	pool, ctx := requireDB(t)
	id := insertMonitoredAction(t, pool, "rolling_back", "", "{}")
	injectOutcomeFault(t, ctx, pool, id, "rolled_back", 1000)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	started := time.Now()
	if err := recordActionOutcome(cctx, pool, nopLog, id, "rolled_back", "x"); err == nil {
		t.Fatal("a cancelled write must fail")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancelled write took %v, want no backoff", elapsed)
	}
}

// An operator rollback whose DDL ran but whose terminal write fails once
// still ends rolled_back.
func TestRollbackAction_TerminalWriteRetried(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	_, index := stuckIndex(t, ctx, pool)
	id := insertMonitoredAction(t, pool, "monitoring",
		"DROP INDEX CONCURRENTLY IF EXISTS "+index, "{}")
	injectOutcomeFault(t, ctx, pool, id, "rolled_back", 1)
	if err := exec.RollbackAction(ctx, id, "operator"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	if indexPresentByName(t, ctx, pool, index) {
		t.Fatal("the rollback did not drop the index")
	}
}

// When every write fails the operator is told, and the reconciler later
// settles the row from the catalog.
func TestRollbackAction_UnrecordedRollbackIsReconciled(t *testing.T) {
	pool, ctx := requireDB(t)
	logs := &outcomeLog{}
	exec := stuckExecutor(t, pool)
	exec.logFn = logs.log
	_, index := stuckIndex(t, ctx, pool)
	id := insertMonitoredAction(t, pool, "monitoring",
		"DROP INDEX CONCURRENTLY IF EXISTS "+index, "{}")
	remove := injectOutcomeFault(t, ctx, pool, id, "rolled_back", 1000)
	err := exec.RollbackAction(ctx, id, "operator")
	if err == nil || !strings.Contains(err.Error(), "record") {
		t.Fatalf("err = %v, want the unrecorded outcome reported", err)
	}
	if !logs.contains(fmt.Sprint(id), "rolled_back") {
		t.Fatalf("log = %v, want the action id and outcome", logs.lines)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolling_back" {
		t.Fatalf("outcome = %q, want rolling_back until reconciled", outcome)
	}
	remove()
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log
		SET measured_at = now() - make_interval(secs => $2) WHERE id = $1`, id,
		(exec.stuckRollbackGrace() + time.Minute).Seconds()); err != nil {
		t.Fatalf("age the claim: %v", err)
	}
	reconcileOnce(t, ctx, exec)
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back (the index is gone)", outcome)
	}
}
