package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A catalog category whose read hit a server-side timeout is read once
// more in the same cycle (dogfood lifeos, 2026-10-04: the queries and
// indexes reads timed out once in hours, then ran in tens of ms). The
// retries of one cycle are bounded, and a permanent failure is never
// retried.

func stmtTimeout() error {
	return fmt.Errorf("collect indexes: %w", &pgconn.PgError{Code: "57014",
		Message: "canceling statement due to statement timeout"})
}

func TestCollect_TimedOutCategoryRetriedOnce(t *testing.T) {
	rec := &logRecorder{}
	c := New(testPool(t), testConfig(), 170000, rec.log)
	var calls atomic.Int32
	c.overrideStep("indexes", func(_ context.Context, s *Snapshot) error {
		if calls.Add(1) == 1 {
			return stmtTimeout()
		}
		s.Indexes = []IndexStats{{IndexRelName: "retried_idx"}}
		return nil
	})
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("indexes read %d times, want 2", calls.Load())
	}
	if !snap.Available("indexes") || len(snap.Indexes) != 1 ||
		snap.Indexes[0].IndexRelName != "retried_idx" {
		t.Fatalf("indexes = %+v / %v, want the retried read", snap.Indexes, snap.Unavailable)
	}
	if !rec.has("INFO", "indexes") || !rec.has("INFO", "retr") {
		t.Fatalf("logs = %v, want a note that indexes was retried", rec.lines)
	}
}

func TestCollect_PersistentTimeoutReadTwiceThenUnavailable(t *testing.T) {
	rec := &logRecorder{}
	c := New(testPool(t), testConfig(), 170000, rec.log)
	var calls atomic.Int32
	c.overrideStep("queries", func(context.Context, *Snapshot) error {
		calls.Add(1)
		return stmtTimeout()
	})
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if calls.Load() != stepAttempts {
		t.Fatalf("queries read %d times, want %d", calls.Load(), stepAttempts)
	}
	if snap.Available("queries") || !strings.Contains(snap.Unavailable["queries"], "57014") {
		t.Fatalf("queries unavailable = %q, want the timeout", snap.Unavailable["queries"])
	}
	if !rec.has("WARN", "queries unavailable") {
		t.Fatalf("logs = %v, want the unavailable warning", rec.lines)
	}
}

func TestCollect_PermanentFailureNotRetried(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	var calls atomic.Int32
	c.overrideStep("foreign_keys", func(context.Context, *Snapshot) error {
		calls.Add(1)
		return &pgconn.PgError{Code: "42501", Message: "permission denied"}
	})
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if calls.Load() != 1 || snap.Available("foreign_keys") {
		t.Fatalf("permission failure read %d times (available %v), want once and unavailable",
			calls.Load(), snap.Available("foreign_keys"))
	}
}

// The cycle's retry budget: three timed-out categories get only
// maxCycleRetries second reads between them.
func TestCollect_RetryBudgetPerCycle(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	var calls atomic.Int32
	for _, name := range []string{"queries", "tables", "indexes"} {
		c.overrideStep(name, func(context.Context, *Snapshot) error {
			calls.Add(1)
			return stmtTimeout()
		})
	}
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if want := int32(3 + maxCycleRetries); calls.Load() != want {
		t.Fatalf("timed-out categories read %d times, want %d (3 + %d retries)",
			calls.Load(), want, maxCycleRetries)
	}
	for _, name := range []string{"queries", "tables", "indexes"} {
		if snap.Available(name) {
			t.Errorf("%s available after failing every read", name)
		}
	}
}

// A retried system read that succeeds keeps the snapshot; a canceled
// cycle is not retried.
func TestCollect_SystemRetriedAndCancelNotRetried(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	var calls atomic.Int32
	c.overrideStep("system", func(_ context.Context, s *Snapshot) error {
		if calls.Add(1) == 1 {
			return &pgconn.PgError{Code: "55P03", Message: "lock timeout"}
		}
		s.System.TotalBackends = 7
		return nil
	})
	snap, err := c.collect(context.Background())
	if err != nil || snap.System.TotalBackends != 7 || calls.Load() != 2 {
		t.Fatalf("collect = %v after %d system reads, want the retried system stats", err,
			calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls.Store(0)
	c.overrideStep("system", func(context.Context, *Snapshot) error {
		calls.Add(1)
		cancel()
		return context.Canceled
	})
	if _, err := c.collect(ctx); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("canceled collect = %v after %d reads, want canceled once", err,
			calls.Load())
	}
}
