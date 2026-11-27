package selfbudget

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a scripted wall clock for TrackFor.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func clockedLoops() (*Loops, *fakeClock) {
	c := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	return NewLoopsWithClock(c.now), c
}

// footprint-07 happy path: every database's collector is its own loop,
// and a process loop (no database in ctx) has no suffix.
func TestTrackFor_DatabasesReportedSeparately(t *testing.T) {
	l, clock := clockedLoops()
	ctx1 := WithDatabase(context.Background(), "db1")
	ctx2 := WithDatabase(context.Background(), "db2")
	_, done1 := l.TrackFor(ctx1, "collector")
	clock.advance(300 * time.Millisecond)
	done1()
	_, done2 := l.TrackFor(ctx2, "collector")
	clock.advance(200 * time.Millisecond)
	done2()
	_, done3 := l.TrackFor(context.Background(), "notify")
	clock.advance(50 * time.Millisecond)
	done3()
	got := l.Snapshot()
	if s := got["collector:db1"]; s.Busy != 300*time.Millisecond || s.Runs != 1 {
		t.Fatalf("collector:db1 = %+v, want 300ms over 1 run", s)
	}
	if s := got["collector:db2"]; s.Busy != 200*time.Millisecond || s.Runs != 1 {
		t.Fatalf("collector:db2 = %+v, want 200ms over 1 run", s)
	}
	if s := got["notify"]; s.Busy != 50*time.Millisecond || s.Runs != 1 {
		t.Fatalf("notify = %+v, want 50ms over 1 run", s)
	}
	if _, mixed := got["collector"]; mixed || len(got) != 3 {
		t.Fatalf("snapshot = %+v, want exactly the three keyed loops", got)
	}
}

// Boundary: a loop that spends 900 of its 1,000 ms waiting on the LLM is
// 100 ms busy; the wait is recorded under llm_wait:<database> and does not
// rank as local work, so a loop with 200 ms of real work tops top_loops.
func TestTrackFor_LLMWaitIsNotBusyTime(t *testing.T) {
	l, clock := clockedLoops()
	ctx := WithDatabase(context.Background(), "db1")
	runCtx, done := l.TrackFor(ctx, "analyzer")
	clock.advance(1000 * time.Millisecond)
	AddWait(runCtx, 900*time.Millisecond) // the fake LLM call
	done()
	_, other := l.TrackFor(ctx, "collector")
	clock.advance(200 * time.Millisecond)
	other()
	snap := l.Snapshot()
	if s := snap["analyzer:db1"]; s.Busy != 100*time.Millisecond || s.Runs != 1 {
		t.Fatalf("analyzer:db1 = %+v, want 100ms busy", s)
	}
	if s := snap["llm_wait:db1"]; s.Busy != 900*time.Millisecond || s.Runs != 1 {
		t.Fatalf("llm_wait:db1 = %+v, want 900ms over 1 run", s)
	}
	top := TopLoops(nil, LocalWork(snap), 5)
	if len(top) != 2 || top[0].Name != "collector:db1" || top[0].BusyMs != 200 ||
		top[1].Name != "analyzer:db1" || top[1].BusyMs != 100 {
		t.Fatalf("top loops = %+v, want collector:db1 200ms then analyzer:db1 100ms", top)
	}
}

// Parallel LLM calls can wait longer than the run's wall time: busy time
// clamps at zero instead of going negative.
func TestTrackFor_WaitAboveWallClampsAtZero(t *testing.T) {
	l, clock := clockedLoops()
	runCtx, done := l.TrackFor(WithDatabase(context.Background(), "db1"), "briefing")
	clock.advance(time.Second)
	AddWait(runCtx, 1500*time.Millisecond)
	done()
	snap := l.Snapshot()
	if s := snap["briefing:db1"]; s.Busy != 0 || s.Runs != 1 {
		t.Fatalf("briefing:db1 = %+v, want 0 busy over 1 run", s)
	}
	if s := snap["llm_wait:db1"]; s.Busy != 1500*time.Millisecond {
		t.Fatalf("llm_wait:db1 = %+v, want the full 1500ms", s)
	}
}

// Nil/empty: a ctx with no tracked run has no Wait; AddWait is a no-op
// and a run without LLM calls records no llm_wait entry (today's
// behaviour).
func TestAddWait_NoRunInContextIsANoOp(t *testing.T) {
	l, clock := clockedLoops()
	AddWait(context.Background(), time.Second)
	AddWait(context.TODO(), -time.Second)
	_, done := l.TrackFor(context.Background(), "retention")
	clock.advance(400 * time.Millisecond)
	done()
	snap := l.Snapshot()
	if len(snap) != 1 || snap["retention"].Busy != 400*time.Millisecond {
		t.Fatalf("snapshot = %+v, want only retention 400ms", snap)
	}
}

// A negative wait is not evidence and is ignored.
func TestAddWait_NegativeIgnored(t *testing.T) {
	l, clock := clockedLoops()
	runCtx, done := l.TrackFor(context.Background(), "facts")
	clock.advance(time.Second)
	AddWait(runCtx, -500*time.Millisecond)
	done()
	snap := l.Snapshot()
	if snap["facts"].Busy != time.Second {
		t.Fatalf("facts = %+v, want the full second", snap["facts"])
	}
	if _, ok := snap["llm_wait"]; ok {
		t.Fatalf("negative wait recorded: %+v", snap)
	}
}

// A run inside another (the tuner inside the analyzer cycle) is its own
// loop; the outer loop's busy time excludes it, so time is counted once.
func TestTrackFor_NestedRunIsExcludedFromItsParent(t *testing.T) {
	l, clock := clockedLoops()
	ctx := WithDatabase(context.Background(), "db1")
	outer, doneOuter := l.TrackFor(ctx, "analyzer")
	clock.advance(300 * time.Millisecond)
	inner, doneInner := l.TrackFor(outer, "tuner")
	clock.advance(400 * time.Millisecond)
	AddWait(inner, 100*time.Millisecond)
	doneInner()
	clock.advance(300 * time.Millisecond)
	doneOuter()
	snap := l.Snapshot()
	if s := snap["analyzer:db1"]; s.Busy != 600*time.Millisecond {
		t.Fatalf("analyzer:db1 = %+v, want 600ms (1000 minus the tuner's 400)", s)
	}
	if s := snap["tuner:db1"]; s.Busy != 300*time.Millisecond {
		t.Fatalf("tuner:db1 = %+v, want 300ms (400 minus 100 LLM wait)", s)
	}
	if s := snap["llm_wait:db1"]; s.Busy != 100*time.Millisecond || s.Runs != 1 {
		t.Fatalf("llm_wait:db1 = %+v, want 100ms once", s)
	}
}

// Ending a run twice counts it once.
func TestTrackFor_DoneIsIdempotent(t *testing.T) {
	l, clock := clockedLoops()
	_, done := l.TrackFor(context.Background(), "executor")
	clock.advance(time.Second)
	done()
	done()
	if s := l.Snapshot()["executor"]; s.Runs != 1 || s.Busy != time.Second {
		t.Fatalf("executor = %+v, want one run of 1s", s)
	}
}

// Concurrent LLM calls of one run all reach its Wait.
func TestAddWait_ConcurrentCallsAccumulate(t *testing.T) {
	l, clock := clockedLoops()
	runCtx, done := l.TrackFor(WithDatabase(context.Background(), "db1"), "sre")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				AddWait(runCtx, time.Millisecond)
			}
		}()
	}
	wg.Wait()
	clock.advance(time.Second)
	done()
	snap := l.Snapshot()
	if s := snap["llm_wait:db1"]; s.Busy != 800*time.Millisecond {
		t.Fatalf("llm_wait:db1 = %+v, want 800ms", s)
	}
	if s := snap["sre:db1"]; s.Busy != 200*time.Millisecond {
		t.Fatalf("sre:db1 = %+v, want 200ms", s)
	}
}

func TestTrackFor_NilLoopsIsSafe(t *testing.T) {
	var l *Loops
	ctx, done := l.TrackFor(context.Background(), "x")
	if ctx == nil {
		t.Fatal("nil loops returned a nil ctx")
	}
	done()
	if s := l.Snapshot(); len(s) != 0 {
		t.Fatalf("nil loops snapshot = %+v", s)
	}
}

func TestSplitLoopName(t *testing.T) {
	for _, c := range []struct{ name, component, database string }{
		{"collector:db1", "collector", "db1"},
		{"collector:db:with:colons", "collector", "db:with:colons"},
		{"notify", "notify", ""},
		{"", "", ""},
	} {
		comp, db := SplitLoopName(c.name)
		if comp != c.component || db != c.database {
			t.Errorf("SplitLoopName(%q) = %q, %q; want %q, %q", c.name, comp, db,
				c.component, c.database)
		}
		if c.name != "" && LoopName(c.component, c.database) != c.name {
			t.Errorf("LoopName(%q, %q) != %q", c.component, c.database, c.name)
		}
	}
}

func TestDatabaseOf(t *testing.T) {
	if got := DatabaseOf(context.Background()); got != "" {
		t.Fatalf("no database = %q", got)
	}
	ctx := WithDatabase(WithDatabase(context.Background(), "outer"), "inner")
	if got := DatabaseOf(ctx); got != "inner" {
		t.Fatalf("DatabaseOf = %q, want the innermost", got)
	}
}

func TestLocalWorkAndForDatabase(t *testing.T) {
	snap := map[string]LoopStat{
		"collector:db1": {Busy: time.Second, Runs: 1},
		"collector:db2": {Busy: time.Second, Runs: 1},
		"llm_wait:db1":  {Busy: time.Hour, Runs: 1},
		"llm_wait":      {Busy: time.Hour, Runs: 1},
		"notify":        {Busy: time.Second, Runs: 1},
	}
	local := LocalWork(snap)
	if len(local) != 3 {
		t.Fatalf("LocalWork = %+v, want the three non-wait loops", local)
	}
	for name := range local {
		if comp, _ := SplitLoopName(name); comp == LLMWaitLoop {
			t.Fatalf("LocalWork kept %q", name)
		}
	}
	db1 := ForDatabase(local, "db1")
	if len(db1) != 1 || db1["collector:db1"].Runs != 1 {
		t.Fatalf("ForDatabase(db1) = %+v, want only collector:db1", db1)
	}
	if got := ForDatabase(nil, "db1"); len(got) != 0 {
		t.Fatalf("ForDatabase(nil) = %+v", got)
	}
	if len(snap) != 5 {
		t.Fatal("LocalWork changed its input")
	}
}
