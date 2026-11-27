package lakebaseobs

import (
	"testing"
	"time"
)

// No concurrent access tests here: the planner functions are pure and take
// cursors by value. Runtime CAS under concurrent writers is a DB test.

func plannerOptions(t *testing.T) Options {
	t.Helper()
	o := Options{WarehouseID: "test"}.defaults()
	if err := o.validate(); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPlannerZeroCursorStartsOneWindowBehindSettleLag(t *testing.T) {
	o := plannerOptions(t)
	now := scheduleEpoch
	w, ok := nextWindow(Cursor{}, forwardPass, now, o)
	wantTo := now.Add(-o.SettleLag)
	if !ok || !w.To.Equal(wantTo) || !w.From.Equal(wantTo.Add(-o.Window)) {
		t.Fatalf("zero cursor window %v..%v ok=%v, want %v..%v", w.From, w.To, ok,
			wantTo.Add(-o.Window), wantTo)
	}
	for _, p := range []pass{shortPass, deepPass} {
		if w, ok := nextWindow(Cursor{}, p, now, o); ok {
			t.Fatalf("pass %d planned %v..%v before any forward read", p, w.From, w.To)
		}
	}
}

func TestPlannerForwardStopsAtSettleLagAndRewindsFutureCursor(t *testing.T) {
	o := plannerOptions(t)
	now := scheduleEpoch
	end := now.Add(-o.SettleLag)
	c := Cursor{ForwardThrough: now.Add(-3 * time.Minute), Width: 10 * time.Minute}
	w, ok := nextWindow(c, forwardPass, now, o)
	if !ok || !w.From.Equal(c.ForwardThrough) || !w.To.Equal(end) {
		t.Fatalf("forward window %v..%v ok=%v, want capped at %v", w.From, w.To, ok, end)
	}
	c.Width = 30 * time.Second
	if w, _ := nextWindow(c, forwardPass, now, o); w.To.Sub(w.From) != 30*time.Second {
		t.Fatalf("forward width %v, want adaptive 30s", w.To.Sub(w.From))
	}
	for _, at := range []time.Time{end, now.Add(-time.Second), now} {
		c.ForwardThrough = at
		if w, ok := nextWindow(c, forwardPass, now, o); ok {
			t.Fatalf("cursor %v inside settle lag read %v..%v", at, w.From, w.To)
		}
	}
	c = Cursor{ForwardThrough: now.Add(time.Hour)}
	w, ok = nextWindow(c, forwardPass, now, o)
	if !ok || !w.From.Equal(end.Add(-o.Window)) || !w.To.Equal(end) {
		t.Fatalf("future cursor not rewound: %v..%v ok=%v", w.From, w.To, ok)
	}
	if got := advance(c, forwardPass, w, now, o); !got.ForwardThrough.Equal(end) {
		t.Fatalf("future cursor kept %v, want rewound to %v", got.ForwardThrough, end)
	}
	if got := forwardWidth(Cursor{Width: 2 * o.Window}, o); got != o.Window {
		t.Fatalf("width above Window not capped: %v", got)
	}
}

func TestPlannerShortSweepPinsTargetAndRestarts(t *testing.T) {
	o := plannerOptions(t)
	now := scheduleEpoch
	top := now.Add(-o.SettleLag)
	c := Cursor{ForwardThrough: top}
	w, ok := nextWindow(c, shortPass, now, o)
	bottom := top.Add(-o.ShortLookback)
	if !ok || !w.From.Equal(bottom) || !w.To.Equal(bottom.Add(o.Window)) {
		t.Fatalf("new short sweep %v..%v ok=%v", w.From, w.To, ok)
	}
	c = advance(c, shortPass, w, now, o)
	if !c.ShortTarget.Equal(top) || !c.ShortThrough.Equal(w.To) {
		t.Fatalf("short sweep state %+v", c)
	}
	later := now.Add(time.Minute)
	c.ForwardThrough = later.Add(-o.SettleLag)
	w2, ok := nextWindow(c, shortPass, later, o)
	if !ok || !w2.From.Equal(w.To) || !w2.To.Equal(w.To.Add(o.Window)) {
		t.Fatalf("in-progress short sweep moved its edge: %v..%v", w2.From, w2.To)
	}
	c.ShortThrough = c.ShortTarget
	w3, ok := nextWindow(c, shortPass, later, o)
	if !ok || !w3.From.Equal(c.ForwardThrough.Add(-o.ShortLookback)) {
		t.Fatalf("finished short sweep did not restart behind the forward edge: %v", w3.From)
	}
	if got := advance(c, shortPass, w3, later, o); !got.ShortTarget.Equal(c.ForwardThrough) {
		t.Fatalf("restart pinned %v, want %v", got.ShortTarget, c.ForwardThrough)
	}
}

func TestPlannerDeepSweepStartsAtMostOncePerDay(t *testing.T) {
	o := plannerOptions(t)
	now := scheduleEpoch
	c := Cursor{ForwardThrough: now.Add(-o.SettleLag)}
	w, ok := nextWindow(c, deepPass, now, o)
	oldest := now.Add(-SourceRetention)
	if !ok || !w.From.Equal(oldest) || w.To.Sub(w.From) != o.Window {
		t.Fatalf("deep start %v..%v ok=%v", w.From, w.To, ok)
	}
	c = advance(c, deepPass, w, now, o)
	if !c.DeepStartedAt.Equal(now) || !c.ReconcileTarget.Equal(c.ForwardThrough) {
		t.Fatalf("deep start state %+v", c)
	}
	c.ReconcileThrough = c.ReconcileTarget
	for _, at := range []time.Time{now.Add(time.Minute), now.Add(deepInterval - time.Second)} {
		if w, ok := nextWindow(c, deepPass, at, o); ok {
			t.Fatalf("deep sweep restarted at %v (%v..%v) before 24h", at, w.From, w.To)
		}
	}
	at := now.Add(deepInterval)
	if w, ok := nextWindow(c, deepPass, at, o); !ok || !w.From.Equal(at.Add(-SourceRetention)) {
		t.Fatalf("deep sweep not restarted after 24h: %v ok=%v", w.From, ok)
	}
	// An unfinished sweep written before this release continues, never restarts.
	legacy := Cursor{ForwardThrough: c.ForwardThrough, ReconcileThrough: now.Add(-time.Hour),
		ReconcileTarget: now.Add(-o.SettleLag)}
	w, ok = nextWindow(legacy, deepPass, now, o)
	if !ok || !w.From.Equal(legacy.ReconcileThrough) {
		t.Fatalf("legacy sweep %v ok=%v", w.From, ok)
	}
}

func TestPlannerReconcileWidthGrowsOnlyForSparseFullWidthStreams(t *testing.T) {
	o := plannerOptions(t)
	if got := reconcileWidth(Cursor{Sparse: sparseReads}, o, o.ShortLookback); got != 4*o.Window {
		t.Fatalf("sparse reconcile width %v, want %v", got, 4*o.Window)
	}
	if got := reconcileWidth(Cursor{Sparse: sparseReads - 1}, o, o.ShortLookback); got != o.Window {
		t.Fatalf("not yet sparse reconcile width %v", got)
	}
	narrow := Cursor{Width: time.Minute, Sparse: sparseReads}
	if got := reconcileWidth(narrow, o, o.ShortLookback); got != time.Minute {
		t.Fatalf("narrowed stream reconcile width %v", got)
	}
	got := reconcileWidth(Cursor{Sparse: sparseReads}, o, 30*time.Minute)
	if got != 30*time.Minute {
		t.Fatalf("reconcile width ignored the pass limit: %v", got)
	}
}

func TestPlannerAdaptCountsSparseReadsAtTheQuarterBoundary(t *testing.T) {
	o := plannerOptions(t)
	full := Window{scheduleEpoch, scheduleEpoch.Add(time.Minute)}
	c := Cursor{Width: time.Minute}
	if got := adapt(c, full, o.MaxRows/4, o); got.Sparse != 0 || got.Width != time.Minute {
		t.Fatalf("read at MaxRows/4 counted as sparse: %+v", got)
	}
	for i := 1; i < sparseReads; i++ {
		c = adapt(c, full, o.MaxRows/4-1, o)
		if c.Sparse != i || c.Width != time.Minute {
			t.Fatalf("sparse read %d state %+v", i, c)
		}
	}
	c = adapt(c, full, o.MaxRows/4-1, o)
	if c.Width != 2*time.Minute || c.Sparse != 0 {
		t.Fatalf("third sparse read did not double width: %+v", c)
	}
	half := Window{scheduleEpoch, scheduleEpoch.Add(time.Minute)}
	c = Cursor{Width: 2 * time.Minute, Sparse: 1}
	// 63 rows in half the width projects to 126, above the 125-row quarter.
	if got := adapt(c, half, o.MaxRows/8+1, o); got.Sparse != 0 {
		t.Fatalf("partial window projected below quarter: %+v", got)
	}
	capped := Cursor{Width: o.Window - time.Second, Sparse: sparseReads - 1}
	nearCap := Window{scheduleEpoch, scheduleEpoch.Add(o.Window - time.Second)}
	if got := adapt(capped, nearCap, 0, o); got.Width != o.Window {
		t.Fatalf("width grew past Window: %v", got.Width)
	}
	atCap := Cursor{Width: o.Window, Sparse: sparseReads}
	got := adapt(atCap, Window{scheduleEpoch, scheduleEpoch.Add(time.Minute)}, 0, o)
	if got.Width != o.Window || got.Sparse != sparseReads {
		t.Fatalf("width at cap changed: %+v", got)
	}
}

func TestPlannerExpireHistoryCountsEachGap(t *testing.T) {
	now := scheduleEpoch
	oldest := now.Add(-SourceRetention)
	c := Cursor{LastPoll: oldest.Add(-time.Hour), ForwardThrough: oldest.Add(-time.Hour),
		ReconcileThrough: oldest.Add(-2 * time.Hour), ReconcileTarget: oldest.Add(time.Hour)}
	got := expireHistory(c, now)
	moved := got.ForwardThrough.Equal(oldest) && got.ReconcileThrough.Equal(oldest)
	if got.GapCount != 3 || got.LastGap == "" || !moved {
		t.Fatalf("expired history %+v", got)
	}
	fresh := Cursor{LastPoll: now.Add(-time.Minute), ForwardThrough: now.Add(-3 * time.Minute)}
	if got := expireHistory(fresh, now); got != fresh {
		t.Fatalf("fresh cursor changed: %+v", got)
	}
	if got := expireHistory(Cursor{}, now); got != (Cursor{}) {
		t.Fatalf("zero cursor changed: %+v", got)
	}
}

func TestOptionsScheduleDefaultsAndBounds(t *testing.T) {
	o := plannerOptions(t)
	if o.SettleLag != 120*time.Second || o.ShortLookback != 2*time.Hour ||
		o.QueriesPerPoll != 2*6+4 {
		t.Fatalf("defaults settle=%v short=%v budget=%d", o.SettleLag, o.ShortLookback,
			o.QueriesPerPoll)
	}
	one := Options{WarehouseID: "test", Streams: []Stream{Plans}}.defaults()
	if one.QueriesPerPoll != 6 {
		t.Fatalf("single-stream budget %d, want 6", one.QueriesPerPoll)
	}
	kept := (Options{WarehouseID: "test", QueriesPerPoll: 3}).defaults()
	if kept.QueriesPerPoll != 3 {
		t.Fatalf("explicit budget overridden: %d", kept.QueriesPerPoll)
	}
	for _, tc := range []struct {
		settle, short time.Duration
		ok            bool
	}{
		{time.Hour, SourceRetention, true}, {time.Second, time.Minute, true},
		{-time.Second, 0, false}, {time.Hour + time.Second, 0, false},
		{0, SourceRetention + time.Second, false}, {0, -time.Second, false},
	} {
		o := Options{WarehouseID: "test", SettleLag: tc.settle, ShortLookback: tc.short}.defaults()
		if err := o.validate(); (err == nil) != tc.ok {
			t.Fatalf("settle=%v short=%v validate=%v, want ok=%v", tc.settle, tc.short, err, tc.ok)
		}
	}
}
