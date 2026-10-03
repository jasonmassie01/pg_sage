package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// Self-exclusion of the executor's decision inputs (perf v1.8.3,
// perf-selfexcl): pg_sage's own statements and sessions never move the
// write-latency regression check, the before-state evidence or the
// runaway detector's blocker counts.

// pg_sage's statements are tagged after their first keyword ("INSERT
// /* pg_sage */ INTO sage.action_log ..."), so a text filter on
// 'INSERT%' matched them: a slow pg_sage write could make a verified
// action look like a write regression and roll it back.
func TestWriteLatencyLeavesOutPgSageStatements(t *testing.T) {
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("public.selfexcl_wl_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (v int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+table) })
	if _, err := pool.Exec(ctx, `SELECT pg_stat_statements_reset(0,
		(SELECT oid FROM pg_database WHERE datname = current_database()), 0)`); err != nil {
		t.Skipf("pg_stat_statements_reset unavailable: %v", err)
	}
	sage := selfload.SagePool(t, testDSN())
	// Another package's pg_stat_statements_reset() on the shared server can
	// erase the inserts' rows before they are read: repeat then.
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		for i := range 3 {
			if _, err := pool.Exec(ctx, "INSERT INTO "+table+" (v) VALUES ($1)", i); err != nil {
				t.Fatalf("application insert: %v", err)
			}
		}
		if _, err := sage.Exec(ctx, "INSERT INTO "+table+
			" (v) SELECT 1 FROM pg_sleep(0.3)"); err != nil {
			t.Fatalf("pg_sage insert: %v", err)
		}
		return writeLatencyProblems(t, ctx, pool, table)
	})
}

// writeLatencyProblems checks the write-latency input equals the mean of
// the application's insert into table (the only application write).
func writeLatencyProblems(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string) []string {
	t.Helper()
	var appMean float64
	err := pool.QueryRow(ctx, `SELECT mean_exec_time FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query LIKE 'INSERT INTO `+table+` (v) VALUES%'`).Scan(&appMean)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{"application statement missing from pg_stat_statements"}
	}
	if err != nil {
		t.Fatalf("application statement in pg_stat_statements: %v", err)
	}
	var got float64
	if err := pool.QueryRow(ctx, writeLatencySQL).Scan(&got); err != nil {
		t.Fatalf("write latency: %v", err)
	}
	if math.Abs(got-appMean) > 1e-9 || got >= 100 {
		return []string{fmt.Sprintf("write latency = %.3f ms, want the application's "+
			"mean %.3f ms (pg_sage's 300 ms insert left out)", got, appMean)}
	}
	return nil
}

func TestBeforeStateActiveBackendsAreApplicationSessions(t *testing.T) {
	requireDB(t)
	sage := selfload.SagePool(t, testDSN())
	selfload.Start(t, testDSN())
	e := New(sage, config.DefaultConfig(), zeroTime(), func(string, string, ...any) {})
	state := e.snapshotBeforeState(context.Background(), nil)
	if got, ok := state["active_backends"].(int); !ok || got != 2 {
		t.Fatalf("before_state active_backends = %v, want 2 (application waiter and "+
			"sleeper; pg_sage's own sessions left out)", state["active_backends"])
	}
}

// before_state's active backends are client sessions: parallel workers
// (and autovacuum workers, logical walsenders) carry the database's name
// and show as active too, but none is a session of the application.
func TestBeforeStateActiveBackendsAreClientSessions(t *testing.T) {
	requireDB(t)
	sage := selfload.SagePool(t, testDSN())
	w := selfload.New(t, testDSN())
	leader := w.StartParallel(t)
	e := New(sage, config.DefaultConfig(), zeroTime(), func(string, string, ...any) {})
	state := e.snapshotBeforeState(context.Background(), nil)
	workers, err := w.Workers(leader)
	if err != nil {
		t.Fatal(err)
	}
	if workers == 0 {
		t.Fatal("the probe's parallel workers ended before the read")
	}
	if got, ok := state["active_backends"].(int); !ok || got != 1 {
		t.Fatalf("before_state active_backends = %v, want 1: the application session, "+
			"not its %d parallel workers", state["active_backends"], workers)
	}
}

// A pg_sage session queued behind an application lock is not a victim
// the runaway policy should terminate the holder for.
func TestRunawayBlockerCountsLeaveOutPgSageWaiters(t *testing.T) {
	requireDB(t)
	sage := selfload.SagePool(t, testDSN())
	_, app, _ := selfload.Start(t, testDSN())
	d := NewRunawayDetector(sage, &config.RunawayConfig{Enabled: true},
		func(string, string, ...any) {})
	counts, err := d.loadBlockerCounts(context.Background())
	if err != nil {
		t.Fatalf("loadBlockerCounts: %v", err)
	}
	if counts[app.Holder] != 1 {
		t.Fatalf("application holder %d blocks %d sessions, want 1 (the application "+
			"waiter; the pg_sage waiter left out)", app.Holder, counts[app.Holder])
	}
}
