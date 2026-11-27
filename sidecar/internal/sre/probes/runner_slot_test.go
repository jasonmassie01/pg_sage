package probes

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// WithSlot (sre-07) lets a read the runner does not make (the
// investigator's plan-only EXPLAIN) hold the same per-database slot and
// sidecar slot as a probe: a full queue is concurrency_limit, never a
// second concurrent read on the database.

func TestRunnerWithSlot_RunsFnHoldingBothSlots(t *testing.T) {
	global := NewLimiter(2)
	r := NewRunner(nil, Catalog(), global)
	ran := false
	err := r.WithSlot(context.Background(), func(ctx context.Context) error {
		ran = true
		if len(r.local.slots) != 1 || len(global.slots) != 1 {
			t.Errorf("slots held = local %d global %d, want 1 and 1",
				len(r.local.slots), len(global.slots))
		}
		if ctx == nil || ctx.Err() != nil {
			t.Errorf("fn ctx = %v", ctx)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("WithSlot = %v ran %v, want nil and true", err, ran)
	}
	if len(r.local.slots) != 0 || len(global.slots) != 0 {
		t.Fatalf("slots after = local %d global %d, want both released",
			len(r.local.slots), len(global.slots))
	}
}

func TestRunnerWithSlot_PropagatesFnErrorAndReleases(t *testing.T) {
	r := NewRunner(nil, Catalog(), NewLimiter(1))
	want := errors.New("explain failed")
	if err := r.WithSlot(context.Background(), func(context.Context) error {
		return want
	}); !errors.Is(err, want) || errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("WithSlot = %v, want the fn's error unwrapped", err)
	}
	if len(r.local.slots) != 0 || len(r.global.slots) != 0 {
		t.Fatal("a failed fn kept its slot")
	}
}

func TestRunnerWithSlot_NilRunnerIsNotConfigured(t *testing.T) {
	var r *Runner
	called := false
	err := r.WithSlot(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	if err == nil || called || errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("nil runner WithSlot = %v called %v, want a not-configured error", err,
			called)
	}
	if r.Pool() != nil || r.AcquireWait() != acquireWait {
		t.Fatalf("nil runner Pool/AcquireWait = %v %s", r.Pool(), r.AcquireWait())
	}
}

func TestRunnerWithSlot_FullQueueIsConcurrencyLimit(t *testing.T) {
	r := NewRunner(nil, Catalog(), NewLimiter(1))
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = r.WithSlot(context.Background(), func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	called := false
	start := time.Now()
	err := r.WithSlot(ctx, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, ErrConcurrencyLimit) || called {
		t.Fatalf("WithSlot behind a held slot = %v called %v, want ErrConcurrencyLimit",
			err, called)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("returned after %s, want it to wait for the slot", waited)
	}
	close(release)
	// Released: the next caller gets the slot (state transition).
	if err := r.WithSlot(context.Background(), func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("WithSlot after release = %v", err)
	}
}

func TestRunnerWithSlot_SharesTheSidecarLimiter(t *testing.T) {
	global := NewLimiter(1)
	a, b := NewRunner(nil, Catalog(), global), NewRunner(nil, Catalog(), global)
	var mu sync.Mutex
	inside, peak := 0, 0
	var wg sync.WaitGroup
	for _, r := range []*Runner{a, b, a, b} {
		wg.Add(1)
		go func(r *Runner) {
			defer wg.Done()
			err := r.WithSlot(context.Background(), func(context.Context) error {
				mu.Lock()
				inside++
				peak = max(peak, inside)
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("WithSlot = %v", err)
			}
		}(r)
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("peak concurrent holders = %d, want 1 (one sidecar slot)", peak)
	}
}

func TestRunnerWithSlot_RunAndWithSlotExcludeEachOther(t *testing.T) {
	r := NewRunner(nil, Catalog(), NewLimiter(4))
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = r.WithSlot(context.Background(), func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)
	release2, err := r.acquire(contextWithTimeout(t, 100*time.Millisecond))
	if err == nil {
		release2()
		t.Fatal("a probe took the database slot WithSlot holds")
	}
}

func contextWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
