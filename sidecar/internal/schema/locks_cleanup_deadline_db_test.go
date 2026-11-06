package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// collection-01 cleanup under its caller's deadline: the meta database
// bootstraps under a 10 s context, so a slow batch (large TOASTed legacy
// rows) must stop on its own statement_timeout before that context ends,
// and the cleanup must still make progress instead of retrying a batch
// that can never finish.

// slowLocksDeletes makes each deleted locks row cost perRow seconds.
func slowLocksDeletes(t *testing.T, pool *pgxpool.Pool, perRow string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		CREATE FUNCTION public.slow_delete() RETURNS trigger LANGUAGE plpgsql AS $f$
		BEGIN PERFORM pg_sleep(`+perRow+`); RETURN OLD; END $f$;
		CREATE TRIGGER slow_delete BEFORE DELETE ON sage.snapshots
		   FOR EACH ROW EXECUTE FUNCTION public.slow_delete()`); err != nil {
		t.Fatalf("slow delete trigger: %v", err)
	}
}

// Boundary: a batch slower than the caller's whole deadline is cut by the
// cleanup's own statement_timeout, the cleanup shrinks its batch, deletes
// some rows and returns nil before the caller's context expires.
func TestLocksCleanup_SlowBatchStaysInsideCallerDeadline(t *testing.T) {
	pool := freshSchemaPool(t, "locks_deadline")
	resetCleanup(t, pool)
	seedLegacyLocks(t, pool, 3000, "1 day")
	slowLocksDeletes(t, pool, "0.002") // one 3,000-row batch: 6 s
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := migrateLocksCleanup(ctx, pool)
	if err != nil || ctx.Err() != nil {
		t.Fatalf("cleanup = %v (caller context %v), want nil before the deadline",
			err, ctx.Err())
	}
	left := countCategory(t, pool, "locks")
	if left == 0 || left >= 3000 || cleanupDone(t, pool) {
		t.Fatalf("%d of 3000 locks rows left (done %v), want some deleted, "+
			"the rest for the next bootstrap", left, cleanupDone(t, pool))
	}
}

// Boundary and nil/zero: a batch's timeout is half the time left, clamped,
// and never more than half of the caller's remaining deadline; with no time
// left it is below locksCleanupNoTime, so no batch starts (statement_timeout
// 0 would mean no timeout at all).
func TestLocksBatchTimeout_Clamps(t *testing.T) {
	bg := context.Background()
	if got := locksBatchTimeout(bg, time.Now().Add(time.Hour)); got != locksCleanupMaxSlice {
		t.Errorf("an hour left: %v, want %v", got, locksCleanupMaxSlice)
	}
	if got := locksBatchTimeout(bg, time.Now().Add(-time.Second)); got != locksCleanupMinSlice {
		t.Errorf("budget spent: %v, want the %v floor", got, locksCleanupMinSlice)
	}
	if got := locksBatchTimeout(bg, time.Now().Add(4*time.Second)); got <= 1900*time.Millisecond ||
		got > 2*time.Second {
		t.Errorf("4 s left: %v, want about 2 s", got)
	}
	short, cancel := context.WithTimeout(bg, time.Second)
	defer cancel()
	if got := locksBatchTimeout(short, time.Now().Add(time.Hour)); got <= 0 ||
		got > 500*time.Millisecond {
		t.Errorf("caller has 1 s: %v, want at most half of it", got)
	}
	gone, cancelGone := context.WithDeadline(bg, time.Now().Add(-time.Second))
	defer cancelGone()
	if got := locksBatchTimeout(gone, time.Now().Add(time.Hour)); got >= locksCleanupNoTime {
		t.Errorf("caller out of time: %v, want below %v (no batch)", got, locksCleanupNoTime)
	}
}

// Boundary: the cleanup's deadline is its budget, or halfway to the
// caller's deadline when that comes first.
func TestLocksCleanupDeadline_HalfOfCallerTime(t *testing.T) {
	start := time.Now()
	if got := locksCleanupDeadline(context.Background()).Sub(start); got < locksCleanupBudget ||
		got > locksCleanupBudget+time.Second {
		t.Errorf("no caller deadline: %v, want the %v budget", got, locksCleanupBudget)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if got := locksCleanupDeadline(ctx).Sub(start); got < 1900*time.Millisecond ||
		got > 2100*time.Millisecond {
		t.Errorf("caller has 4 s: %v, want about 2 s", got)
	}
	long, cancelLong := context.WithTimeout(context.Background(), time.Hour)
	defer cancelLong()
	if got := locksCleanupDeadline(long).Sub(start); got > locksCleanupBudget+time.Second {
		t.Errorf("caller has an hour: %v, want the %v budget", got, locksCleanupBudget)
	}
}
