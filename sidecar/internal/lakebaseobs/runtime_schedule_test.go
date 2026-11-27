package lakebaseobs

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// No concurrent access tests here: collection runs under Poll's pollMu on
// one goroutine. Concurrent cursor writers are covered by the DB CAS test.

func TestScheduleBusyStreamsKeepForwardWithinSettleLag(t *testing.T) {
	f := &rateReader{every: 100 * time.Millisecond} // 10 rows/s on every stream
	rt := scheduleRuntime(t, f, nil)
	o := rt.options
	mem := newMemCursors()
	now := scheduleEpoch
	for poll := 0; poll < 120; poll++ {
		before := f.count()
		errs := pollAt(rt, mem, f, now)
		reads := f.readsSince(before)
		if len(reads) > o.QueriesPerPoll {
			t.Fatalf("poll %d spent %d reads, budget %d", poll, len(reads), o.QueriesPerPoll)
		}
		per := map[Stream]int{}
		for _, r := range reads {
			per[r.stream]++
		}
		for _, s := range o.Streams {
			if per[s] == 0 {
				t.Fatalf("poll %d: stream %s did not read", poll, s)
			}
		}
		if poll >= 100 && len(errs) != 0 {
			t.Fatalf("poll %d still failing in steady state: %v", poll, errs)
		}
		now = now.Add(o.PollInterval)
	}
	assertForwardSteady(t, rt, mem, f, now.Add(-o.PollInterval))
}

// assertForwardSteady checks every stream trails last by SettleLag up to one
// poll, adapted its width below MaxRows, and read every row it passed.
func assertForwardSteady(t *testing.T, rt *Runtime, mem *memCursors, f *rateReader,
	last time.Time) {
	t.Helper()
	o := rt.options
	limit := o.SettleLag + o.PollInterval
	for _, st := range rt.Status() {
		c := mem.cursor(st.Stream)
		lag := last.Sub(c.ForwardThrough)
		if lag > limit || lag < o.SettleLag {
			t.Fatalf("%s forward lag %v, want %v..%v", st.Stream, lag, o.SettleLag, limit)
		}
		if st.ForwardLagSeconds == nil || *st.ForwardLagSeconds > int64(limit/time.Second) {
			t.Fatalf("%s status lag %v, want at most %v", st.Stream, st.ForwardLagSeconds, limit)
		}
		if c.Width <= 0 || c.Width > 50*time.Second {
			t.Fatalf("%s width %v did not adapt below 500 rows at 10 rows/s", st.Stream, c.Width)
		}
		// A stream whose first read hit the budget starts at a later poll's
		// zero-cursor origin, so coverage starts at its first successful read.
		first := rowIndex(firstRead(f, st.Stream), f.every)
		end := rowIndex(c.ForwardThrough, f.every)
		for k := first; k < end; k++ {
			if !mem.seen[string(st.Stream)+"-"+strconv.FormatInt(k, 10)] {
				t.Fatalf("%s forward coverage skipped row %d", st.Stream, k)
			}
		}
	}
}

func firstRead(f *rateReader, s Stream) time.Time {
	for _, r := range f.readsSince(0) {
		if r.stream == s && r.err == nil {
			return r.w.From
		}
	}
	return time.Time{}
}

func TestScheduleWidthShrinksOnBudgetAndGrowsBackWhenSparse(t *testing.T) {
	f := &rateReader{every: 100 * time.Millisecond}
	rt := scheduleRuntime(t, f, func(o *Options) { o.Streams = []Stream{Plans} })
	o := rt.options
	mem := newMemCursors()
	now := scheduleEpoch
	pollAt(rt, mem, f, now)
	if got := mem.cursor(Plans).Width; got != o.Window/32 {
		t.Fatalf("width after budget halving %v, want %v", got, o.Window/32)
	}
	f.mu.Lock()
	f.every = 0
	f.mu.Unlock()
	previous := mem.cursor(Plans).Width
	for poll := 1; poll <= 40 && mem.cursor(Plans).Width < o.Window; poll++ {
		now = now.Add(o.PollInterval)
		if errs := pollAt(rt, mem, f, now); len(errs) != 0 {
			t.Fatalf("sparse poll %d: %v", poll, errs)
		}
		// Catch-up can take several reads per poll, so a poll may double twice.
		got := mem.cursor(Plans).Width
		if got < previous || (got != o.Window && (got%previous != 0 || !powerOfTwo(got/previous))) {
			t.Fatalf("width moved %v -> %v, want doubling", previous, got)
		}
		previous = got
	}
	if got := mem.cursor(Plans).Width; got != o.Window {
		t.Fatalf("sparse stream width %v never grew back to %v", got, o.Window)
	}
}

func powerOfTwo(n time.Duration) bool { return n > 0 && n&(n-1) == 0 }

func TestScheduleBudgetExhaustionRemembersNarrowedWidthWithoutCommit(t *testing.T) {
	f := &rateReader{every: 100 * time.Millisecond}
	rt := scheduleRuntime(t, f, func(o *Options) {
		o.Streams = []Stream{Plans}
		o.QueriesPerPoll = 2
	})
	mem := newMemCursors()
	errs := pollAt(rt, mem, f, scheduleEpoch)
	if len(errs) != 1 || !errors.Is(errs[0], ErrBudget) || len(mem.commits) != 0 {
		t.Fatalf("saturated poll errs=%v commits=%d", errs, len(mem.commits))
	}
	before := f.count()
	pollAt(rt, mem, f, scheduleEpoch.Add(time.Minute))
	reads := f.readsSince(before)
	if got := reads[0].w.To.Sub(reads[0].w.From); got != rt.options.Window/4 {
		t.Fatalf("next poll restarted at %v, want remembered %v", got, rt.options.Window/4)
	}
}

func TestScheduleLateRowInsideSettleLagIsReadByForwardPass(t *testing.T) {
	f := &rateReader{every: time.Minute}
	rt := scheduleRuntime(t, f, nil)
	mem := newMemCursors()
	now := scheduleEpoch
	for poll := 0; poll < 10; poll++ {
		pollAt(rt, mem, f, now)
		now = now.Add(time.Minute)
	}
	at := now.Add(15 * time.Second)
	f.mu.Lock()
	f.late = append(f.late, lateRow{Plans, at, at.Add(90 * time.Second), "late-90s"})
	f.mu.Unlock()
	for poll := 0; poll < 4 && !mem.seen["late-90s"]; poll++ {
		pollAt(rt, mem, f, now)
		now = now.Add(time.Minute)
	}
	if !mem.seen["late-90s"] || mem.firstPass["late-90s"] != forwardPass {
		t.Fatalf("90s-late row seen=%v pass=%d, want forward", mem.seen["late-90s"],
			mem.firstPass["late-90s"])
	}
}

func TestScheduleThirtyMinuteLateRowIsReadByShortPassWithinTwoPolls(t *testing.T) {
	for _, warm := range []int{10, 11, 12} {
		f := &rateReader{every: time.Minute}
		rt := scheduleRuntime(t, f, nil)
		mem := newMemCursors()
		now := scheduleEpoch
		for poll := 0; poll < warm; poll++ {
			pollAt(rt, mem, f, now)
			now = now.Add(time.Minute)
		}
		visible := now.Add(-time.Second)
		f.mu.Lock()
		f.late = append(f.late, lateRow{Logs, visible.Add(-30 * time.Minute), visible, "late-30m"})
		f.mu.Unlock()
		polls := 0
		for ; polls < 2 && !mem.seen["late-30m"]; polls++ {
			pollAt(rt, mem, f, now)
			now = now.Add(time.Minute)
		}
		if !mem.seen["late-30m"] || mem.firstPass["late-30m"] != shortPass {
			t.Fatalf("warm=%d: 30m-late row seen=%v pass=%d after %d polls, want short within 2",
				warm, mem.seen["late-30m"], mem.firstPass["late-30m"], polls)
		}
	}
}

func TestScheduleDeepSweepOnlyUsesBudgetLeftAfterForward(t *testing.T) {
	f := &rateReader{every: time.Minute}
	rt := scheduleRuntime(t, f, nil)
	o := rt.options
	mem := newMemCursors()
	now := scheduleEpoch
	deepReads, idlePolls := 0, 0
	for poll := 0; poll < 400 && idlePolls < 10; poll++ {
		before, commits := f.count(), len(mem.commits)
		if errs := pollAt(rt, mem, f, now); len(errs) != 0 {
			t.Fatalf("poll %d: %v", poll, errs)
		}
		if reads := f.count() - before; reads > o.QueriesPerPoll {
			t.Fatalf("poll %d spent %d reads, budget %d", poll, reads, o.QueriesPerPoll)
		}
		got := mem.commits[commits:]
		forward, deep := 0, 0
		for i, c := range got {
			switch {
			case c.pass == forwardPass && i >= len(o.Streams):
				t.Fatalf("poll %d: forward commit %d ran after another pass", poll, i)
			case c.pass == forwardPass:
				forward++
			case c.pass == deepPass:
				deep++
			}
		}
		if poll > 0 && forward != len(o.Streams) {
			t.Fatalf("poll %d: %d forward commits, want one per stream", poll, forward)
		}
		deepReads += deep
		if deep == 0 && deepReads > 0 {
			idlePolls++
		}
		now = now.Add(o.PollInterval)
	}
	if idlePolls < 10 || deepReads == 0 {
		t.Fatalf("deep sweep never finished: reads=%d idle=%d", deepReads, idlePolls)
	}
	for _, st := range rt.Status() {
		c := mem.cursor(st.Stream)
		incomplete := c.ReconcileThrough.Before(c.ReconcileTarget)
		if c.DeepStartedAt.IsZero() || incomplete || st.ReconcileProgress != 1 {
			t.Fatalf("%s deep sweep incomplete: %+v progress=%v", st.Stream, c,
				st.ReconcileProgress)
		}
	}
}

func TestScheduleBudgetBelowStreamCountRotatesEveryStream(t *testing.T) {
	f := &rateReader{every: time.Minute}
	rt := scheduleRuntime(t, f, func(o *Options) { o.QueriesPerPoll = 3 })
	mem := newMemCursors()
	now := scheduleEpoch
	for poll := 0; poll < 6; poll += 2 {
		before := f.count()
		pollAt(rt, mem, f, now)
		pollAt(rt, mem, f, now.Add(time.Minute))
		reads := f.readsSince(before)
		if len(reads) > 6 {
			t.Fatalf("two polls spent %d reads, budget 3 each", len(reads))
		}
		per := map[Stream]bool{}
		for _, r := range reads {
			per[r.stream] = true
		}
		if len(per) != len(rt.options.Streams) {
			t.Fatalf("polls %d-%d starved streams: read %v", poll, poll+1, per)
		}
		now = now.Add(2 * time.Minute)
	}
}

func TestScheduleReaderFailureIsolatesOneStream(t *testing.T) {
	down := errors.New("synthetic warehouse unavailable")
	f := &rateReader{every: time.Minute, fail: map[Stream]error{Logs: down}}
	rt := scheduleRuntime(t, f, nil)
	mem := newMemCursors()
	errs := pollAt(rt, mem, f, scheduleEpoch)
	if len(errs) != 1 || !errors.Is(errs[0], down) {
		t.Fatalf("errors %v, want only the failed stream", errs)
	}
	for _, st := range rt.Status() {
		c := mem.cursor(st.Stream)
		switch {
		case st.Stream == Logs && (st.LastError == "" || !c.ForwardThrough.IsZero()):
			t.Fatalf("failed stream status %+v cursor %+v", st, c)
		case st.Stream != Logs && (st.LastError != "" || c.ForwardThrough.IsZero()):
			t.Fatalf("healthy stream %s blocked by another: %+v", st.Stream, st)
		}
	}
}

// edgeReader returns a row stamped at the window's exclusive upper edge.
type edgeReader struct{ hash string }

func (r *edgeReader) ReadWindow(_ context.Context, s Stream, _ Scope, w Window) (Batch, error) {
	return Batch{Events: []Event{{ID: "edge", Stream: s, ScopeHash: r.hash, ComputeID: "c",
		OccurredAt: w.To, ReceivedAt: w.To, Version: SchemaVersion}}}, nil
}

func TestScheduleOutOfWindowEventIsRejectedWithoutCommit(t *testing.T) {
	reader := &edgeReader{}
	rt, err := NewRuntime(reader, &Store{}, testScope(), Options{WarehouseID: "test",
		Streams: []Stream{Plans}})
	if err != nil {
		t.Fatal(err)
	}
	reader.hash = rt.scope.Hash()
	mem := newMemCursors()
	errs := rt.collect(context.Background(), mem, scheduleEpoch)
	if len(errs) != 1 || !errors.Is(errs[0], ErrScope) || len(mem.commits) != 0 {
		t.Fatalf("out-of-window event errs=%v commits=%d", errs, len(mem.commits))
	}
	if st := rt.Status()[0]; st.LastError != ErrScope.Error() {
		t.Fatalf("status error %q, want scope error", st.LastError)
	}
}
