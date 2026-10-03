package selfload

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// StartParallel runs a parallel scan from one application session until
// the test ends and returns the session's pid once parallel workers run
// beside it. Workers carry the database's name and show as active like
// autovacuum workers and logical walsenders, but none is a client
// session: a deterministic stand-in for the backends a session count
// must leave out.
func (w *Workload) StartParallel(t testing.TB) int {
	t.Helper()
	table := "public.selfload_par_" + w.suffix
	fn := "public.selfload_par_slow_" + w.suffix
	for _, sql := range []string{
		"CREATE TABLE " + table + " (id int) WITH (parallel_workers = 2, " +
			"autovacuum_enabled = off)",
		"INSERT INTO " + table + " SELECT g FROM generate_series(1, 2000) g",
		"CREATE FUNCTION " + fn + "(int) RETURNS boolean LANGUAGE plpgsql " +
			"PARALLEL SAFE COST 1000000 AS " +
			"'BEGIN PERFORM pg_sleep(0.5); RETURN true; END'",
	} {
		if _, err := w.admin.Exec(context.Background(), sql); err != nil {
			t.Fatalf("selfload: parallel fixture: %v", err)
		}
	}
	w.drops = append(w.drops, "DROP TABLE IF EXISTS "+table,
		"DROP FUNCTION IF EXISTS "+fn+"(int)")
	scan := "SELECT count(*) AS selfload_parallel FROM " + table + " WHERE " + fn + "(id)"
	for range 10 {
		pid, stop := w.runParallel(t, scan)
		if w.awaitWorkers(t, pid, 3*time.Second) {
			t.Cleanup(stop)
			return pid
		}
		// Worker slots are shared by every package on the server: all
		// taken when the scan starts leaves it serial. Start it again.
		stop()
	}
	t.Fatal("selfload: no parallel worker started in 10 attempts " +
		"(max_parallel_workers taken on the shared server)")
	return 0
}

// runParallel starts scan on a new application session that plans it in
// parallel; stop cancels it and closes the session.
func (w *Workload) runParallel(t testing.TB, scan string) (int, func()) {
	t.Helper()
	conn := connect(t, w.DSN, AppName, false)
	for _, set := range []string{"SET parallel_setup_cost = 0",
		"SET parallel_tuple_cost = 0", "SET max_parallel_workers_per_gather = 2"} {
		if _, err := conn.Exec(context.Background(), set); err != nil {
			t.Fatalf("selfload: %s: %v", set, err)
		}
	}
	pid := pidOf(t, conn)
	w.pids = append(w.pids, pid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, scan)
		done <- err
	}()
	return pid, func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("selfload: parallel scan ended with %v", err)
		}
		_ = conn.Close(context.Background())
	}
}

// awaitWorkers reports whether parallel workers of leader run within
// timeout.
func (w *Workload) awaitWorkers(t testing.TB, leader int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := w.Workers(leader)
		if err != nil {
			t.Fatalf("selfload: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Workers counts the active parallel workers of leader.
func (w *Workload) Workers(leader int) (int, error) {
	var n int
	err := w.admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE leader_pid = $1 AND pid <> $1 AND state = 'active'`, leader).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count parallel workers of %d: %w", leader, err)
	}
	return n, nil
}
