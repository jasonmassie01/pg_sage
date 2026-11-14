package testdb

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ci-14 on the test server: DBNow reads the server's clock_timestamp(),
// and a ClockWatch over the pool measures how far it moves against the
// host.

// clockPool is a pool on a fresh database (closing it twice is safe).
func clockPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return statsPool(t, CreateDatabase(t, "clock"))
}

// Happy path: the database clock is the host's to within the VM's drift.
// The offset is logged, not asserted tightly: the Docker Desktop VM clock
// slews by up to 2.5 s. An hour or more apart is a time zone or scan bug.
func TestDBNowReadsTheServerClock(t *testing.T) {
	p := clockPool(t)
	before := time.Now()
	at, err := DBNow(context.Background(), p)
	after := time.Now()
	if err != nil {
		t.Fatalf("DBNow: %v", err)
	}
	if at.IsZero() {
		t.Fatal("DBNow returned the zero time")
	}
	mid := before.Add(after.Sub(before) / 2)
	offset := at.Sub(mid)
	t.Logf("database clock - host clock = %v (within 1 s: %v)", offset,
		offset.Abs() <= time.Second)
	if offset.Abs() > time.Minute {
		t.Fatalf("database clock %v is %v from the host's %v", at, offset, mid)
	}
	// clock_timestamp moves inside one transaction; now() would not.
	tx, err := p.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	first := MustDBNow(t, tx)
	time.Sleep(20 * time.Millisecond)
	if second := MustDBNow(t, tx); !second.After(first) {
		t.Fatalf("DBNow in one transaction: %v then %v, want the clock to move", first,
			second)
	}
}

// Invalid input: a closed pool is an error that names clock_timestamp.
func TestDBNowOnAClosedPool(t *testing.T) {
	p := clockPool(t)
	p.Close()
	at, err := DBNow(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "clock_timestamp") {
		t.Fatalf("DBNow on a closed pool = %v, %v, want an error naming clock_timestamp",
			at, err)
	}
	if !at.IsZero() {
		t.Fatalf("DBNow returned %v with its error", at)
	}
	if w, err := StartClockWatch(context.Background(), p); err == nil || w != nil ||
		!strings.Contains(err.Error(), "clock watch") {
		t.Fatalf("StartClockWatch on a closed pool: watch=%v err=%v", w, err)
	}
}

// Integration: a watch on the real server measures a step, and a pool
// closed under it makes Widen fail its caller with that error.
func TestClockWatchOnTheServer(t *testing.T) {
	p := clockPool(t)
	w, err := StartClockWatch(context.Background(), p)
	if err != nil {
		t.Fatalf("StartClockWatch: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	step, err := w.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	t.Logf("database clock step over 200ms: %v", step)
	if step.Abs() > 10*time.Second {
		t.Fatalf("step %v over 200ms: the two clocks are not being compared", step)
	}
	if got := w.Widen(t, time.Second); got < time.Second {
		t.Fatalf("Widen(1s) = %v, want at least 1s", got)
	}
	p.Close()
	got, rec := runRecorded(t, func(tb testing.TB) time.Duration {
		return w.Widen(tb, time.Second)
	})
	if got != 0 || len(rec.fatals) != 1 ||
		!strings.Contains(rec.fatals[0], "clock_timestamp") {
		t.Fatalf("Widen on a closed pool = %v, fatals %q, want one naming clock_timestamp",
			got, rec.fatals)
	}
}
