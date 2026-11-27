package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-04 integration: a real collector, a real pg_stat_reset() (the
// statistics epoch) and analyzer restarts on a fake clock. Before
// rules-04 a restart in the middle of the window restarted the zero-scan
// clock, and a restart with an open finding resolved it.

func (f *resetFixture) restartAnalyzer() {
	cfg := f.a.cfg
	f.a = New(f.pool, cfg, f.coll, nil, nil, nil, nil, noopLog)
	f.a.extras.Now = func() time.Time { return f.now }
}

func (f *resetFixture) countUnused(t *testing.T, ident, status string) int {
	t.Helper()
	return countFindingRows(t, f.pool, "unused_index", ident, status)
}

// resetStats calls pg_stat_reset() and returns the new statistics epoch.
func (f *resetFixture) resetStats(t *testing.T) time.Time {
	t.Helper()
	ctx := context.Background()
	var reset time.Time
	if _, err := f.pool.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("pg_stat_reset: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT stats_reset FROM pg_stat_database
		WHERE datname = current_database()`).Scan(&reset); err != nil {
		t.Fatalf("stats_reset: %v", err)
	}
	return reset
}

// requireNoneResolved is CHECK-15: both findings are open, none resolved.
func (f *resetFixture) requireNoneResolved(t *testing.T) {
	t.Helper()
	for _, ident := range []string{"public.ix_seen", "public.ix_unseen"} {
		if n := f.countUnused(t, ident, "resolved"); n != 0 {
			t.Fatalf("CHECK-15: %s resolved %d times after a restart with an empty clock",
				ident, n)
		}
		if n := f.countUnused(t, ident, "open"); n != 1 {
			t.Fatalf("CHECK-15: %s has %d open rows, want 1", ident, n)
		}
	}
}

func zeroSample(after time.Time) func(*collector.Snapshot) bool {
	return func(s *collector.Snapshot) bool {
		return !s.System.RelationStatsEpoch.Before(after) &&
			snapScans(s, "ix_seen") == 0 && snapScans(s, "ix_unseen") == 0
	}
}

// CHECK-14: across simulated restarts an open zero-scan finding keeps its
// sage.findings id and its occurrence_count grows. CHECK-15: the first
// cycle after a restart with an empty clock table (and no history)
// resolves no unused_index row.
func TestUnusedClock_RestartMidWindowKeepsFindingIdentity(t *testing.T) {
	f := newResetFixture(t)
	ctx := context.Background()
	reset := f.resetStats(t)
	f.waitSnapshot(t, "a zero-scan sample after the reset", zeroSample(reset))
	f.now = reset.Add(time.Minute)
	f.a.cycle(ctx)
	f.waitSnapshot(t, "a mid-window sample", zeroSample(reset))
	f.now = reset.Add(4 * day)
	f.a.cycle(ctx)

	f.restartAnalyzer() // mid-window
	f.waitSnapshot(t, "a sample after the restart", zeroSample(reset))
	f.now = reset.Add(7*day + 2*time.Minute)
	f.a.cycle(ctx)
	if open, _ := f.openUnused(t); len(open) != 2 {
		t.Fatalf("restart mid-window: open findings %v, want both indexes", open)
	}
	id, count := openFindingID(t, f.pool, "public.ix_unseen")

	f.restartAnalyzer()
	f.waitSnapshot(t, "a sample after the second restart", zeroSample(reset))
	f.now = reset.Add(7*day + 3*time.Minute)
	f.a.cycle(ctx)
	id2, count2 := openFindingID(t, f.pool, "public.ix_unseen")
	if id2 != id || count2 <= count {
		t.Fatalf("CHECK-14: id %d->%d, occurrence %d->%d; want the same id, a higher count",
			id, id2, count, count2)
	}

	f.stop() // no new snapshot rows while the history is removed
	if _, err := f.pool.Exec(ctx, `DELETE FROM sage.unused_index_clock;
		DELETE FROM sage.snapshots WHERE category = 'indexes'`); err != nil {
		t.Fatalf("empty the clock and the history: %v", err)
	}
	f.restartAnalyzer()
	f.now = reset.Add(7*day + 4*time.Minute)
	f.a.cycle(ctx)
	f.requireNoneResolved(t)
	if id3, _ := openFindingID(t, f.pool, "public.ix_unseen"); id3 != id {
		t.Fatalf("held finding changed id %d -> %d", id, id3)
	}
}
