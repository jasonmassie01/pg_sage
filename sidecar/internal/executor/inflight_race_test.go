package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrency: 20 parallel Apply calls (half finish on their own, half block
// in a statement) and a stop. Every run that started is cancelled or
// finished, every refused one never executed, and nothing starts after the
// latch. -race is unavailable on this machine (no cgo); the invariants are
// asserted directly.
func TestInFlightParallelApplyAndStop(t *testing.T) {
	e := inflightExecutor()
	const runs = 20
	var executed atomic.Int32
	var wg sync.WaitGroup
	results := startParallelRuns(e, runs, &executed, &wg)
	waitWatched(t, e, runs/4)
	ids, err := e.CancelInFlight("emergency stop")
	if err != nil {
		t.Fatalf("CancelInFlight: %v", err)
	}
	if e.inflight.count() != 0 {
		t.Fatalf("%d runs still registered after the stop returned", e.inflight.count())
	}
	afterStop := executed.Load()
	waitDone(t, &wg)
	close(results)
	if executed.Load() != afterStop {
		t.Fatalf("%d runs executed after the stop latched", executed.Load()-afterStop)
	}
	for r := range results {
		switch {
		case r.err == nil && r.blocking:
			t.Fatal("a blocking run returned without being cancelled")
		case r.err != nil && !errors.Is(r.err, ErrEmergencyStop):
			t.Fatalf("run ended with %v, want success or ErrEmergencyStop", r.err)
		}
	}
	if len(ids) < runs/4 {
		t.Fatalf("cancelled ids = %v, want at least the %d watched runs", ids, runs/4)
	}
	var late atomic.Int32
	if _, err := e.Apply(context.Background(),
		blockingIntent(newFakeStatement(), 1, &late)); !errors.Is(err, ErrEmergencyStop) ||
		late.Load() != 0 {
		t.Fatalf("Apply after stop: err=%v executed=%d, want refused", err, late.Load())
	}
}

type parallelResult struct {
	blocking bool
	err      error
}

// startParallelRuns starts n Apply calls: even ones block in a statement
// until a stop cancels it, odd ones finish on their own.
func startParallelRuns(
	e *Executor, n int, executed *atomic.Int32, wg *sync.WaitGroup,
) chan parallelResult {
	results := make(chan parallelResult, n)
	for i := 0; i < n; i++ {
		blocking := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			intent := blockingIntent(newFakeStatement(), int64(100+i), executed)
			if !blocking {
				intent.Execute = func(context.Context, ActionPolicyDecision) (int64, error) {
					executed.Add(1)
					return int64(100 + i), nil
				}
			}
			_, err := e.Apply(context.Background(), intent)
			results <- parallelResult{blocking: blocking, err: err}
		}()
	}
	return results
}

func waitDone(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runs still blocked 5s after the stop")
	}
}

// Rollback runs register too: a stop cancels a running rollback statement
// and the rollback fails with the stop as its reason.
func TestEmergencyStopCancelsRunningRollback(t *testing.T) {
	e := inflightExecutor()
	stmt := newFakeStatement()
	cfg := e.rollbackMonitorConfig(nil)
	cfg.Acquire = nil
	cfg.execRollback = func(ctx context.Context, sql string, _ time.Duration,
		_ ...DDLOption) error {
		return runStatement(ctx, stmt, sql)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := executeRollbackSQL(context.Background(), nil,
			"DROP INDEX CONCURRENTLY public.idx_a", cfg)
		errCh <- err
	}()
	waitWatched(t, e, 1)
	if _, err := e.CancelInFlight("emergency stop"); err != nil {
		t.Fatalf("CancelInFlight: %v", err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrEmergencyStop) {
			t.Fatalf("rollback error = %v, want ErrEmergencyStop", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rollback still running 2s after the stop")
	}
	_, err := executeRollbackSQL(context.Background(), nil, "DROP INDEX public.idx_b", cfg)
	if !errors.Is(err, ErrEmergencyStop) {
		t.Fatalf("rollback after stop = %v, want refused by the latch", err)
	}
}
