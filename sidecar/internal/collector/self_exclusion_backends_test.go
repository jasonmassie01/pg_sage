package collector

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// active_backends and idle_in_transaction count the application's client
// sessions. Backends that are not client sessions also carry the
// database's name in pg_stat_activity and show as active: autovacuum
// workers (they run on the package database between tests; one made
// TestSystemStatsCountApplicationSessionsOnly see 3 active backends on
// PG18 in CI), logical walsenders and parallel workers. A parallel query
// is the deterministic stand-in: one application session, its workers
// active beside it.

const parallelAppName = "collector_parallel_app"

// startParallelQuery runs a parallel scan from one application session
// until the test ends and returns its pid once workers run beside it.
func startParallelQuery(t *testing.T) int {
	t.Helper()
	admin := testPool(t)
	mustExec(t, admin, `CREATE TABLE IF NOT EXISTS public.parallel_probe (id int)
		WITH (parallel_workers = 2, autovacuum_enabled = off)`)
	mustExec(t, admin, `INSERT INTO public.parallel_probe
		SELECT g FROM generate_series(1, 2000) g`)
	mustExec(t, admin, `CREATE OR REPLACE FUNCTION public.parallel_probe_slow(int)
		RETURNS boolean LANGUAGE plpgsql PARALLEL SAFE COST 1000000 AS
		'BEGIN PERFORM pg_sleep(0.5); RETURN true; END'`)
	for attempt := 1; attempt <= 10; attempt++ {
		pid, stop := runParallelScan(t)
		if waitForWorkers(t, pid, 3*time.Second) {
			t.Cleanup(stop)
			return pid
		}
		// Worker slots are shared with every package on the server: all
		// taken at the scan's start leaves it serial. Start it again.
		stop()
	}
	t.Fatal("no parallel worker started for the probe scan in 10 attempts " +
		"(max_parallel_workers exhausted on the shared server)")
	return 0
}

// runParallelScan starts the scan on a new application session; stop
// cancels it and closes the session.
func runParallelScan(t *testing.T) (int, func()) {
	t.Helper()
	conf, err := pgx.ParseConfig(os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	conf.RuntimeParams["application_name"] = parallelAppName
	conn, err := pgx.ConnectConfig(context.Background(), conf)
	if err != nil {
		t.Fatalf("connect application session: %v", err)
	}
	for _, set := range []string{"SET parallel_setup_cost = 0",
		"SET parallel_tuple_cost = 0", "SET max_parallel_workers_per_gather = 2"} {
		if _, err := conn.Exec(context.Background(), set); err != nil {
			t.Fatalf("%s: %v", set, err)
		}
	}
	pid := int(conn.PgConn().PID())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, `SELECT count(*) FROM public.parallel_probe
			WHERE public.parallel_probe_slow(id)`)
		done <- err
	}()
	return pid, func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("parallel probe scan ended with %v", err)
		}
		_ = conn.Close(context.Background())
	}
}

// waitForWorkers reports whether parallel workers of leader run within
// timeout.
func waitForWorkers(t *testing.T, leader int, timeout time.Duration) bool {
	t.Helper()
	pool := testPool(t)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var workers int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE leader_pid = $1 AND pid <> $1 AND state = 'active'`, leader).Scan(&workers)
		if err != nil {
			t.Fatalf("count parallel workers: %v", err)
		}
		if workers > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestSystemStatsCountClientSessionsNotTheirWorkers(t *testing.T) {
	c, pool := sageCollector(t)
	leader := startParallelQuery(t)
	var workers int
	s, err := c.collectSystem(context.Background())
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE leader_pid = $1 AND pid <> $1`, leader).Scan(&workers); err != nil {
		t.Fatalf("count parallel workers: %v", err)
	}
	if workers == 0 {
		t.Fatal("the probe's parallel workers ended before the read")
	}
	if s.ActiveBackends != 1 {
		t.Fatalf("active_backends = %d, want 1: the application session, not its %d "+
			"parallel workers", s.ActiveBackends, workers)
	}
	if s.IdleInTransaction != 0 {
		t.Fatalf("idle_in_transaction = %d, want 0", s.IdleInTransaction)
	}
}
