package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

const (
	stopCICTable = "executor06_cic"
	stopCICIndex = "executor06_cic_idx"
)

// holdOldSnapshot opens a REPEATABLE READ transaction with a snapshot, which
// CREATE INDEX CONCURRENTLY must wait out before it can finish.
func holdOldSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire snapshot holder: %v", err)
	}
	if _, err := conn.Exec(ctx, `BEGIN ISOLATION LEVEL REPEATABLE READ;
		SELECT count(*) FROM public.`+stopCICTable); err != nil {
		conn.Release()
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "ROLLBACK")
		conn.Release()
	})
}

// waitCICBlocked waits until the CIC session is active and waiting on a lock
// (the virtual xid of the snapshot holder).
func waitCICBlocked(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cicSessions(t, ctx, pool, true) > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("CREATE INDEX CONCURRENTLY never waited on the open snapshot")
}

func cicSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waiting bool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND state = 'active'
		  AND query LIKE 'CREATE INDEX CONCURRENTLY `+stopCICIndex+`%'
		  AND (NOT $1 OR wait_event_type = 'Lock')`, waiting).Scan(&n)
	if err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return n
}

func createStopCICTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.`+stopCICTable+`;
		CREATE TABLE public.`+stopCICTable+` (id int);
		INSERT INTO public.`+stopCICTable+` SELECT generate_series(1, 1000)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.`+stopCICTable)
	})
}

// cicIntent runs a CIC finding through Apply on the finding path's own
// statement runner and action recorder.
func cicIntent(e *Executor, f analyzer.Finding) ActionIntent {
	return ActionIntent{
		Authorize: allowExecute, SlotHeld: true,
		Execute: func(ctx context.Context, decision ActionPolicyDecision) (int64, error) {
			execErr := e.runFindingSQL(ctx, f, decision)
			return e.logActionWithDecision(ctx, f, 0, nil, 0, execErr), nil
		},
	}
}

// Integration (CHECK-02): a CIC waits behind an open snapshot; a stop
// cancels it within a second, the action row is failed with reason
// "emergency stop" naming the INVALID index and its cleanup statement, and
// no pg_sage DDL session is left running.
func TestEmergencyStopCancelsRunningCICOnPostgres(t *testing.T) {
	pool, ctx := requireDB(t)
	createStopCICTable(t, ctx, pool)
	holdOldSnapshot(t, ctx, pool)
	e := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	f := analyzer.Finding{Category: "missing_index", Title: "executor-06 stop CIC",
		ObjectIdentifier: "public." + stopCICTable,
		RecommendedSQL: "CREATE INDEX CONCURRENTLY " + stopCICIndex +
			" ON public." + stopCICTable + " (id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS public." + stopCICIndex}
	type outcome struct {
		id  int64
		err error
		at  time.Time
	}
	done := make(chan outcome, 1)
	go func() {
		id, err := e.Apply(ctx, cicIntent(e, f))
		done <- outcome{id, err, time.Now()}
	}()
	waitCICBlocked(t, ctx, pool)

	stoppedAt := time.Now()
	ids, err := e.CancelInFlight("requested by dba")
	if err != nil {
		t.Fatalf("CancelInFlight: %v", err)
	}
	if elapsed := time.Since(stoppedAt); elapsed > 2*time.Second {
		t.Fatalf("stop returned after %s, want under 2s", elapsed)
	}
	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply still running 5s after the stop")
	}
	if took := got.at.Sub(stoppedAt); took > time.Second {
		t.Fatalf("CIC ended %s after the stop, want within 1s", took)
	}
	if got.err != nil || got.id <= 0 || len(ids) != 1 || ids[0] != got.id {
		t.Fatalf("Apply = (%d, %v), cancelled ids %v; want the recorded row's id",
			got.id, got.err, ids)
	}
	assertStoppedCICRow(t, ctx, pool, got.id)
	if n := cicSessions(t, ctx, pool, false); n != 0 {
		t.Fatalf("%d CIC sessions still active after the stop", n)
	}
}

func assertStoppedCICRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	var outcome, reason string
	if err := pool.QueryRow(ctx, `SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id = $1`, id).Scan(&outcome, &reason); err != nil {
		t.Fatalf("read action row %d: %v", id, err)
	}
	qualified := "public." + stopCICIndex
	if outcome != "failed" || !strings.HasPrefix(reason, "emergency stop") ||
		!strings.Contains(reason, "INVALID index "+qualified) ||
		!strings.Contains(reason, "DROP INDEX CONCURRENTLY IF EXISTS "+qualified) {
		t.Fatalf("row %d = %s %q, want failed / emergency stop naming %s and its cleanup",
			id, outcome, reason, qualified)
	}
	var invalid bool
	if err := pool.QueryRow(ctx, `SELECT NOT indisvalid FROM pg_index
		WHERE indexrelid = to_regclass($1)`, qualified).Scan(&invalid); err != nil {
		t.Fatalf("read remnant %s: %v", qualified, err)
	}
	if !invalid {
		t.Fatalf("%s is valid; the cancelled build should have left it INVALID", qualified)
	}
}
