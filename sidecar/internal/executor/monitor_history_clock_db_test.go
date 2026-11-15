package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// freshPool is a pool of its own on the test server, closed at cleanup.
func freshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Under history.store: meta the shift reads the meta database's clock:
// on one server the two clocks agree, and a meta database that cannot be
// read is a failed shift naming it.
func TestHistoryShift_ReadsTheMetaDatabaseClock(t *testing.T) {
	_, ctx := requireDB(t)
	monitored, meta := freshPool(t), freshPool(t)
	store, err := histstore.NewMeta(meta, 7)
	if err != nil {
		t.Fatalf("meta store: %v", err)
	}
	unregister := histstore.Register(monitored, "monitored", store)
	t.Cleanup(unregister)

	d, err := historyShift(ctx, monitored)
	if err != nil || d < -time.Second || d > time.Second {
		t.Fatalf("historyShift = %s, %v; want the same server's clocks within 1s", d, err)
	}

	meta.Close()
	d, err = historyShift(ctx, monitored)
	if err == nil || !strings.Contains(err.Error(), "meta database clock") || d != 0 {
		t.Fatalf("historyShift with the meta database closed = %s, %v; want an error "+
			"naming the meta database clock", d, err)
	}
}

// A monitored database keeps its own history: no shift and no clock read,
// even on a pool that can no longer be read.
func TestHistoryShift_MonitoredHistoryReadsNoClock(t *testing.T) {
	requireDB(t)
	pool := freshPool(t)
	pool.Close()

	d, err := historyShift(context.Background(), pool)
	if err != nil || d != 0 {
		t.Fatalf("historyShift = %s, %v; want 0, nil without a read", d, err)
	}
}

// unreadableMetaHistory is a fresh monitored pool whose history is in a
// meta database that can no longer be read.
func unreadableMetaHistory(t *testing.T) *pgxpool.Pool {
	t.Helper()
	monitored, meta := freshPool(t), freshPool(t)
	store, err := histstore.NewMeta(meta, 7)
	if err != nil {
		t.Fatalf("meta store: %v", err)
	}
	t.Cleanup(histstore.Register(monitored, "monitored", store))
	meta.Close()
	return monitored
}

// The monitor's sample reads go through the history clock: a meta clock
// that cannot be read stops the read before any sample is read.
func TestMonitorObserver_AlignsWithTheMetaClock(t *testing.T) {
	_, ctx := requireDB(t)
	monitored := unreadableMetaHistory(t)

	got, err := RollbackMonitorConfig{}.observer(monitored)(ctx, []int64{1},
		time.Now().Add(-time.Hour), time.Now())

	if err == nil || !strings.Contains(err.Error(), "meta database clock") || got != nil {
		t.Fatalf("observe = %v, %v; want the meta clock read to fail first", got, err)
	}
}

// The frozen pre-action baseline reads through the history clock too.
func TestFreezeBaseline_AlignsWithTheMetaClock(t *testing.T) {
	_, ctx := requireDB(t)
	e := configTestExecutor(unreadableMetaHistory(t))
	var logged []string
	e.logFn = func(_, format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	got := e.freezeBaseline(ctx, verify.ClassIndexCreate, []int64{1})

	if got != nil || !strings.Contains(strings.Join(logged, "\n"), "meta database clock") {
		t.Fatalf("baseline = %v, log %q; want no baseline and the meta clock failure", got,
			logged)
	}
}
