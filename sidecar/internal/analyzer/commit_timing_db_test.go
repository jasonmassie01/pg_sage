package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// collection-10b: a slow_commit rule over pg_stat_statements was planned
// (sum the COMMIT entries' interval time, fire at a 20 ms mean). It is
// not built, because pg_stat_statements does not time the commit: its
// utility hook times only COMMIT's ProcessUtility, which marks the block
// for commit; the commit itself (deferred triggers, the WAL flush, the
// synchronous replication wait) runs afterwards in finish_xact_command.
// This test pins that on the server under test. If a future PostgreSQL
// times the commit work, it fails, and the rule becomes possible.

// No concurrent access test: one connection drives the commits serially,
// and serializeAcrossPackages keeps other suites off the server.

const commitTimingTrigger = 30 * time.Millisecond

// commitCounters reads the summed COMMIT/END calls and time of this
// database's pg_stat_statements entries.
func commitCounters(t *testing.T, pool *pgxpool.Pool) (int64, float64) {
	t.Helper()
	var calls int64
	var totalMs float64
	err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(calls), 0)::int8,
		COALESCE(sum(total_exec_time), 0)::float8 FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query ~* '^\s*(commit|end)\s*;?\s*$'`).Scan(&calls, &totalMs)
	if err != nil {
		t.Fatalf("read the COMMIT entries of pg_stat_statements: %v", err)
	}
	return calls, totalMs
}

// commitTimingTable makes a table whose deferred constraint trigger
// sleeps 30 ms at commit, and skips when pg_stat_statements cannot track
// COMMIT on this server.
func commitTimingTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements cannot be created on this server: %v", err)
	}
	var track string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(
		current_setting('pg_stat_statements.track_utility', true), '')`).Scan(&track); err != nil {
		t.Fatalf("read pg_stat_statements.track_utility: %v", err)
	}
	if track != "on" {
		t.Skipf("pg_stat_statements.track_utility = %q: COMMIT is not tracked", track)
	}
	for _, sql := range []string{`DROP TABLE IF EXISTS public.c10b_commit_timing`,
		`CREATE TABLE public.c10b_commit_timing (x int)`,
		`CREATE OR REPLACE FUNCTION public.c10b_slow_commit() RETURNS trigger
		 LANGUAGE plpgsql AS $$BEGIN PERFORM pg_sleep(0.03); RETURN NULL; END$$`,
		`CREATE CONSTRAINT TRIGGER c10b_slow AFTER INSERT ON public.c10b_commit_timing
		 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		 EXECUTE FUNCTION public.c10b_slow_commit()`} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("set up the slow-commit table: %s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.c10b_commit_timing;
			DROP FUNCTION IF EXISTS public.c10b_slow_commit()`)
	})
}

func TestPgStatStatements_CommitTimeExcludesCommitWork(t *testing.T) {
	pool := phase2Pool(t)
	commitTimingTable(t, pool)
	ctx := context.Background()
	const n = 20
	calls0, total0 := commitCounters(t, pool)
	var wall time.Duration
	for i := 0; i < n; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO public.c10b_commit_timing VALUES (1)"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("insert %d: %v", i, err)
		}
		start := time.Now()
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		wall += time.Since(start)
	}
	calls1, total1 := commitCounters(t, pool)
	if got := calls1 - calls0; got < n {
		t.Fatalf("pg_stat_statements counted %d COMMIT calls, want at least %d", got, n)
	}
	if mean := wall / n; mean < commitTimingTrigger {
		t.Fatalf("client-side commit mean %v, want at least the trigger's %v", mean,
			commitTimingTrigger)
	}
	mean := (total1 - total0) / float64(calls1-calls0)
	if mean >= 5 {
		t.Fatalf("pg_stat_statements COMMIT mean %.3f ms now includes the commit work "+
			"(trigger %v): a slow_commit rule over pg_stat_statements is possible", mean,
			commitTimingTrigger)
	}
}
