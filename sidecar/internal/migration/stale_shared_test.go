package migration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// countingResolver returns a resolver whose UPDATE is a counter and whose
// clock the test moves.
func countingResolver(
	err error, logFn func(string, string, ...any),
) (*StaleResolver, *atomic.Int32, *time.Time) {
	var calls atomic.Int32
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r := &StaleResolver{logFn: logFn, now: func() time.Time { return now }}
	r.resolve = func(context.Context) (int64, error) {
		calls.Add(1)
		return 1, err
	}
	return r, &calls, &now
}

// Concurrent access (migration-12): both detectors call the shared
// resolver from their own goroutines; one interval runs one UPDATE.
func TestStaleResolverSharedRunsOncePerInterval(t *testing.T) {
	r, calls, _ := countingResolver(nil, quietLog)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.maybeRun(context.Background())
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver ran %d times in one interval, want 1", got)
	}
}

// Boundary: just under the interval does not run again; at the interval
// it does.
func TestStaleResolverIntervalBoundary(t *testing.T) {
	r, calls, now := countingResolver(nil, quietLog)
	ctx := context.Background()
	r.maybeRun(ctx)
	*now = now.Add(staleCheckInterval - time.Nanosecond)
	r.maybeRun(ctx)
	if got := calls.Load(); got != 1 {
		t.Fatalf("ran %d times before the interval elapsed, want 1", got)
	}
	*now = now.Add(time.Nanosecond)
	r.maybeRun(ctx)
	if got := calls.Load(); got != 2 {
		t.Fatalf("ran %d times at the interval, want 2", got)
	}
}

// Error propagation: a failed resolve is logged as a warning naming the
// cause and is not retried until the next interval (no hot loop).
func TestStaleResolverFailureWarnsAndWaits(t *testing.T) {
	var mu sync.Mutex
	var warnings []string
	logFn := func(level, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if level == "warn" {
			warnings = append(warnings, format)
		}
	}
	r, calls, _ := countingResolver(errors.New("connection refused"), logFn)
	r.maybeRun(context.Background())
	r.maybeRun(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("failed resolver ran %d times in one interval, want 1", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "migration") {
		t.Fatalf("warnings = %q, want one migration warning", warnings)
	}
}

// Nil/empty: a nil resolver and a pool-less one are no-ops.
func TestStaleResolverNilIsNoop(t *testing.T) {
	var r *StaleResolver
	r.maybeRun(context.Background())
	if got := NewStaleResolver(nil, quietLog); got != nil {
		t.Fatalf("NewStaleResolver(nil pool) = %+v, want nil", got)
	}
}

// State: both detectors hold the resolver they were given; a finding
// sink alone no longer builds a private resolver per detector.
func TestDetectorsShareGivenResolver(t *testing.T) {
	r, _, _ := countingResolver(nil, quietLog)
	cfg := &config.MigrationConfig{Enabled: true, ActivityPolling: true, LogDetection: true}
	adv := NewAdvisor(nil, cfg, 170000, "orders", quietLog, nil)
	det := NewDetector(nil, adv, cfg, quietLog).
		WithFindingSink(&recordingFindingSink{}).WithStaleResolver(r)
	logDet := NewLogDetector(adv, quietLog).
		WithFindingSink(&recordingFindingSink{}).WithStaleResolver(r)
	if det.StaleResolver() != r || logDet.StaleResolver() != r {
		t.Fatalf("detectors hold %p and %p, want %p", det.StaleResolver(),
			logDet.StaleResolver(), r)
	}
	bare := NewDetector(nil, adv, cfg, quietLog).
		WithFindingSink(&recordingFindingSink{})
	if bare.StaleResolver() != nil {
		t.Fatal("WithFindingSink built a private resolver")
	}
}
