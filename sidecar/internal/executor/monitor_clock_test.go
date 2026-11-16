package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// executor-31: the post-action monitor bounds its windows by the clock
// that stamped what it judges (the database's), and uses the host clock
// only when that read fails, with the reason.
//
// No concurrent-access tests here: readMonitorTime and poolClock keep no
// state; each call reads its clock once.

func fixedClock(at time.Time) dbClock {
	return func(context.Context) (time.Time, error) { return at, nil }
}

func failingClock(err error) dbClock {
	return func(context.Context) (time.Time, error) { return time.Time{}, err }
}

func TestReadMonitorTime_UsesTheDatabaseClock(t *testing.T) {
	db := time.Date(2026, 11, 15, 12, 0, 0, 123_000, time.UTC)
	got := readMonitorTime(context.Background(), fixedClock(db))
	if !got.at.Equal(db) || got.source != clockSourceDatabase || got.fallback != "" {
		t.Fatalf("readMonitorTime = %+v, want %s from the database clock, no fallback", got, db)
	}
}

func TestReadMonitorTime_FailedReadFallsBackToTheHostWithTheReason(t *testing.T) {
	before := time.Now()
	got := readMonitorTime(context.Background(),
		failingClock(errors.New("connection refused by test")))
	after := time.Now()
	if got.source != clockSourceHost {
		t.Fatalf("source = %q, want host", got.source)
	}
	if got.at.Before(before) || got.at.After(after) {
		t.Fatalf("fallback time %s outside the host's [%s, %s]", got.at, before, after)
	}
	for _, want := range []string{"connection refused by test", "host clock"} {
		if !strings.Contains(got.fallback, want) {
			t.Fatalf("fallback reason %q does not say %q", got.fallback, want)
		}
	}
}

func TestReadMonitorTime_ZeroTimeIsAFailedRead(t *testing.T) {
	got := readMonitorTime(context.Background(), fixedClock(time.Time{}))
	if got.source != clockSourceHost || got.at.IsZero() ||
		!strings.Contains(got.fallback, "returned no time") {
		t.Fatalf("readMonitorTime(zero) = %+v, want a host fallback naming the empty read", got)
	}
}

func TestReadMonitorTime_NilClockFallsBack(t *testing.T) {
	got := readMonitorTime(context.Background(), nil)
	if got.source != clockSourceHost || got.at.IsZero() ||
		!strings.Contains(got.fallback, "no database clock") {
		t.Fatalf("readMonitorTime(nil) = %+v, want a host fallback naming the missing clock",
			got)
	}
}

func TestReadMonitorTime_CancelledReadFallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clock := func(ctx context.Context) (time.Time, error) {
		return time.Time{}, fmt.Errorf("read the database clock: %w", ctx.Err())
	}
	got := readMonitorTime(ctx, clock)
	if got.source != clockSourceHost || !strings.Contains(got.fallback, "context canceled") {
		t.Fatalf("readMonitorTime(cancelled) = %+v, want a host fallback naming the cancel",
			got)
	}
}

func TestPoolClock_NilPoolIsAnActionableError(t *testing.T) {
	at, err := poolClock(nil)(context.Background())
	if err == nil || !at.IsZero() {
		t.Fatalf("poolClock(nil) = %s, %v; want an error and no time", at, err)
	}
	for _, want := range []string{"clock_timestamp()", "no database pool"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
}

func TestMonitorTimeEvidence_NamesTheClock(t *testing.T) {
	db := time.Date(2026, 11, 15, 12, 0, 0, 0, time.FixedZone("CST", -6*3600))
	ev := monitorTime{at: db, source: clockSourceDatabase}.evidence()
	if ev["source"] != clockSourceDatabase || ev["at"] != db.UTC() {
		t.Fatalf("database evidence = %v, want source database at %s", ev, db.UTC())
	}
	if _, ok := ev["reason"]; ok {
		t.Fatalf("database evidence carries a fallback reason: %v", ev)
	}
	host := readMonitorTime(context.Background(), failingClock(errors.New("down")))
	ev = host.evidence()
	if ev["source"] != clockSourceHost || !strings.Contains(fmt.Sprint(ev["reason"]), "down") {
		t.Fatalf("host evidence = %v, want source host with the reason", ev)
	}
}

func TestRollbackMonitorConfigDatabaseClock_PrefersTheSeam(t *testing.T) {
	db := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := RollbackMonitorConfig{clock: fixedClock(db)}
	at, err := cfg.databaseClock(nil)(context.Background())
	if err != nil || !at.Equal(db) {
		t.Fatalf("databaseClock with a seam = %s, %v; want %s", at, err, db)
	}
	if _, err := (RollbackMonitorConfig{}).databaseClock(nil)(context.Background()); err == nil {
		t.Fatal("databaseClock without a seam or pool read a time; want the pool's error")
	}
}

type clockLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *clockLog) log(_ string, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *clockLog) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestExecutorDatabaseNow_UsesTheSeamAndLogsAFallback(t *testing.T) {
	db := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	logs := &clockLog{}
	e := &Executor{clock: fixedClock(db), logFn: logs.log}
	if got := e.databaseNow(context.Background(), "freeze baseline"); !got.Equal(db) {
		t.Fatalf("databaseNow = %s, want the database clock %s", got, db)
	}
	if logs.joined() != "" {
		t.Fatalf("a database read logged a fallback: %q", logs.joined())
	}
	e.clock = failingClock(errors.New("permission denied for test"))
	before := time.Now()
	got := e.databaseNow(context.Background(), "freeze baseline")
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("fallback databaseNow = %s, want the host clock", got)
	}
	line := logs.joined()
	for _, want := range []string{"freeze baseline", "permission denied for test", "host"} {
		if !strings.Contains(line, want) {
			t.Fatalf("fallback log %q does not say %q", line, want)
		}
	}
}

// Boundary: verificationExpired at exactly window+grace is not expired;
// one nanosecond past it is. The caller passes the database clock.
func TestVerificationExpired_BoundaryOnTheDatabaseClock(t *testing.T) {
	executed := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	window := 2 * time.Hour
	edge := executed.Add(window + window)
	if verificationExpired(executed, window, edge) {
		t.Fatal("expired exactly at window + grace")
	}
	if !verificationExpired(executed, window, edge.Add(time.Nanosecond)) {
		t.Fatal("not expired one nanosecond past window + grace")
	}
}
