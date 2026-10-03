package selfload

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent-access test: a Workload belongs to one test goroutine.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "testsupport_selfload"))
}

func liveDSN(t *testing.T) string {
	t.Helper()
	return testdb.SkipUnlessLive(t)
}

func sessionState(t *testing.T, w *Workload, pid int) (app, state, wait string) {
	t.Helper()
	err := w.admin.QueryRow(context.Background(), `SELECT application_name,
		COALESCE(state, ''), COALESCE(wait_event_type, '') || '/' ||
		COALESCE(wait_event, '') FROM pg_stat_activity WHERE pid = $1`, pid).
		Scan(&app, &state, &wait)
	if err != nil {
		t.Fatalf("state of %d: %v", pid, err)
	}
	return app, state, wait
}

func TestStartPutsEverySessionInItsState(t *testing.T) {
	w, app, sage := Start(t, liveDSN(t))
	for _, g := range []struct {
		group *Group
		name  string
	}{{app, AppName}, {sage, "pg_sage"}} {
		if a, s, _ := sessionState(t, w, g.group.Holder); a != g.name ||
			s != "idle in transaction" {
			t.Errorf("holder %d: %q %q, want %q idle in transaction", g.group.Holder, a, s,
				g.name)
		}
		if _, s, wait := sessionState(t, w, g.group.Waiter); s != "active" ||
			!strings.HasPrefix(wait, "Lock/") {
			t.Errorf("waiter %d: %q %q, want active waiting on a lock", g.group.Waiter, s, wait)
		}
		if _, s, wait := sessionState(t, w, g.group.Sleeper); s != "active" ||
			wait != "Timeout/PgSleep" {
			t.Errorf("sleeper %d: %q %q, want active in pg_sleep", g.group.Sleeper, s, wait)
		}
	}
	if got := len(sage.Pids()); got != 3 {
		t.Fatalf("pg_sage group has %d sessions, want 3", got)
	}
}

func TestStartParallelRunsWorkersBesideOneSession(t *testing.T) {
	w := New(t, liveDSN(t))
	leader := w.StartParallel(t)
	workers, err := w.Workers(leader)
	if err != nil {
		t.Fatal(err)
	}
	if workers < 1 {
		t.Fatalf("leader %d has %d parallel workers, want at least 1", leader, workers)
	}
	if a, s, _ := sessionState(t, w, leader); a != AppName || s != "active" {
		t.Fatalf("leader %d: %q %q, want %q active", leader, a, s, AppName)
	}
}

// After a test ends, none of its workload's sessions and fixtures remain.
func TestCleanupEndsEverySessionAndDropsTheTables(t *testing.T) {
	dsn := liveDSN(t)
	var pids []int
	var tables []string
	t.Run("workload", func(t *testing.T) {
		w := New(t, dsn)
		app := w.StartApp(t)
		sage := w.StartSage(t, 2)
		w.StartParallel(t)
		pids = append(append(append(pids, app.Pids()...), sage.Pids()...), w.pids...)
		tables = []string{w.AppTable, w.SageTable, "public.selfload_par_" + w.suffix}
	})
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var left, kept int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE pid = ANY($1)`, pids).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM unnest($1::text[]) n
		WHERE to_regclass(n) IS NOT NULL`, tables).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if left != 0 || kept != 0 {
		t.Fatalf("after the test: %d of %d sessions still running, %d tables kept",
			left, len(pids), kept)
	}
}

// A session that outlives its client (here: one the workload never
// closes) is terminated by the cleanup after the grace period.
func TestAwaitGoneTerminatesALingeringSession(t *testing.T) {
	dsn := liveDSN(t)
	w := New(t, dsn)
	stray := connect(t, dsn, AppName, false)
	defer func() { _ = stray.Close(context.Background()) }()
	pid := pidOf(t, stray)
	done := make(chan error, 1)
	go func() {
		_, err := stray.Exec(context.Background(), "SELECT pg_sleep(600)")
		done <- err
	}()
	w.pids = append(w.pids, pid)
	started := time.Now()
	w.awaitGone(t)
	if w.waitGone(t, 0) != 0 {
		t.Fatal("the lingering session survived awaitGone")
	}
	if elapsed := time.Since(started); elapsed < 2*time.Second {
		t.Fatalf("terminated after %s, want the 2 s grace period first", elapsed)
	}
	if err := <-done; err == nil {
		t.Fatal("the terminated session's statement succeeded")
	}
}

func TestMinOverReturnsTheSmallestSample(t *testing.T) {
	samples := []float64{5, 3, 7}
	i := 0
	got := MinOver(t, 3, func() (float64, error) {
		v := samples[i]
		i++
		return v, nil
	})
	if got != 3 || i != 3 {
		t.Fatalf("MinOver = %v after %d samples, want 3 after 3", got, i)
	}
}

func TestWorkersReportsAQueryError(t *testing.T) {
	w := New(t, liveDSN(t))
	_ = w.admin.Close(context.Background())
	defer func() { w.admin = connect(t, w.DSN, "selfload_admin", false) }()
	if _, err := w.Workers(1); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Workers on a closed session = %v, want a query error", err)
	}
}
