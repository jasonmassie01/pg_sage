package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Withheld-admission counting (perf-selfexcl). PostgreSQL's
// INSERT ... ON CONFLICT DO UPDATE is atomic under concurrency (9,600
// concurrent upserts lost none in the investigation); a withhold went
// uncounted when the upsert itself failed, and recordWithheldAdmission
// logged the error, reported "new" and the count was gone. Under a loaded
// test run that failure was a pooled connection another test had
// terminated ("terminating connection due to administrator command",
// 2,265 times in that run's server log).

// smallPoolAdmission is an admission fixture on its own pool, so the test
// can terminate exactly that pool's connections.
func smallPoolAdmission(t *testing.T, conns int32) (admissionFixture, *pgxpool.Pool) {
	t.Helper()
	f := newAdmissionFixture(t, learningAdmission())
	cfg, err := pgxpool.ParseConfig(testDSN())
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = conns, 0
	cfg.ConnConfig.RuntimeParams["application_name"] = "withheld_small_pool"
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatalf("small pool: %v", err)
	}
	t.Cleanup(pool.Close)
	exec := New(pool, config.DefaultConfig(), zeroTime(), f.exec.logFn)
	exec.WithDatabaseName(f.database)
	exec.indexVerification = newVerifiedIndexLifecycle(
		&staticVerifier{admission: learningAdmission()}, &fakeVerifiedIndexActions{})
	f.exec = exec
	return f, pool
}

// warmAndTerminate opens n connections in pool, returns them, and has the
// server terminate them; the pool still holds them as idle.
func warmAndTerminate(t *testing.T, f admissionFixture, pool *pgxpool.Pool, n int) {
	t.Helper()
	held := make([]*pgxpool.Conn, 0, n)
	for range n {
		c, err := pool.Acquire(f.ctx)
		if err != nil {
			t.Fatalf("warm pool: %v", err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Release()
	}
	if _, err := f.pool.Exec(f.ctx, `SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity WHERE application_name = 'withheld_small_pool'`); err != nil {
		t.Fatalf("terminate pool connections: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var left int
		_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = 'withheld_small_pool'`).Scan(&left)
		if left == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Every concurrent withhold is counted, also when the pool's idle
// connections were killed under it (pgxpool reuses a connection idle for
// less than a second without a ping).
func TestConcurrentWithholdsCountEveryWithholdOnKilledConnections(t *testing.T) {
	const workers = 16
	f, pool := smallPoolAdmission(t, 4)
	warmAndTerminate(t, f, pool, 4)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.exec.admitIndexBuild(f.ctx, "finding:42", 0); !errors.Is(err,
				ErrVerificationUnavailable) {
				t.Errorf("admission error = %v", err)
			}
		}()
	}
	wg.Wait()
	rows, occurrences, _ := withheldRows(t, f)
	if rows != 1 || occurrences != workers {
		t.Fatalf("rows=%d occurrences=%d, want 1/%d; logs: %s", rows, occurrences,
			workers, strings.Join(*f.logs, " | "))
	}
	if got := f.countLogs("withheld autonomous index build"); got != 1 {
		t.Fatalf("withheld log lines = %d, want 1 (the first withhold only)", got)
	}
}

// An upsert that cannot be written returns its error with what was being
// recorded, and is never reported as a new withhold.
func TestRecordWithheldAdmissionReturnsTheUpsertError(t *testing.T) {
	f := newAdmissionFixture(t, learningAdmission())
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	inserted, err := f.exec.recordWithheldAdmission(ctx, "finding:77", 0, learningAdmission())
	if err == nil || inserted {
		t.Fatalf("recordWithheldAdmission on a cancelled context = %v, %v; want an error, "+
			"not new", inserted, err)
	}
	if !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "record withheld admission finding:77") ||
		!strings.Contains(err.Error(), learningAdmission().Reason) {
		t.Fatalf("error %q lacks context (finding key, reason) or the cause", err)
	}
	if rows, _, _ := withheldRows(t, f); rows != 0 {
		t.Fatalf("withheld rows = %d after a failed record, want 0", rows)
	}
}

// admitIndexBuild still returns the withheld verdict when recording it
// fails, and logs the failure (not the "withheld" line) with context.
func TestAdmitIndexBuildLogsARecordFailure(t *testing.T) {
	f := newAdmissionFixture(t, learningAdmission())
	f.exec.indexVerification = newVerifiedIndexLifecycle(
		&staticVerifier{admission: learningAdmission()}, &fakeVerifiedIndexActions{})
	if _, err := f.pool.Exec(f.ctx, "ALTER TABLE sage.admission_withheld "+
		"RENAME TO admission_withheld_away"); err != nil {
		t.Fatalf("hide table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "ALTER TABLE sage.admission_withheld_away "+
			"RENAME TO admission_withheld")
	})
	err := f.exec.admitIndexBuild(f.ctx, "finding:88", 0)
	if !isAdmissionWithheld(err) {
		t.Fatalf("admitIndexBuild = %v, want the withheld verdict", err)
	}
	if f.countLogs("record withheld admission finding:88") != 1 ||
		f.countLogs("withheld autonomous index build") != 0 {
		t.Fatalf("logs = %q, want one record failure and no withheld line", *f.logs)
	}
}
