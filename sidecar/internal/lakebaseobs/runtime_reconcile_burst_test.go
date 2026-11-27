package lakebaseobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// burstReader is a rateReader whose history holds one dense burst: any read
// wider than dense that overlaps it exceeds the row budget.
type burstReader struct {
	*rateReader
	from, to time.Time
	dense    time.Duration
}

func (b *burstReader) ReadWindow(ctx context.Context, s Stream, sc Scope, w Window) (Batch, error) {
	if w.From.Before(b.to) && w.To.After(b.from) && w.To.Sub(w.From) > b.dense {
		b.mu.Lock()
		b.reads = append(b.reads, readRecord{s, w, ErrBudget})
		b.mu.Unlock()
		return Batch{}, ErrBudget
	}
	return b.rateReader.ReadWindow(ctx, s, sc, w)
}

// A sparse stream reconciles 4 windows per read. When the deep sweep reaches
// an old burst, the read must keep narrowing across polls instead of retrying
// the same too-wide window forever.
func TestScheduleDeepSweepNarrowsAcrossPollsThroughAnOldBurst(t *testing.T) {
	f := &rateReader{every: time.Minute}
	start := scheduleEpoch.Add(-SourceRetention + 3*time.Hour)
	b := &burstReader{rateReader: f, from: start, to: start.Add(10 * time.Minute),
		dense: 5 * time.Minute}
	rt, err := NewRuntime(b, &Store{}, testScope(), Options{WarehouseID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f.hash, f.maxRows = rt.scope.Hash(), rt.options.MaxRows
	mem := newMemCursors()
	now := scheduleEpoch
	var lastErrs []error
	for poll := 0; poll < 120; poll++ {
		lastErrs = pollAt(rt, mem, f, now)
		now = now.Add(rt.options.PollInterval)
	}
	for _, s := range rt.options.Streams {
		c := mem.cursor(s)
		if !c.ReconcileThrough.After(b.to) {
			t.Errorf("%s deep sweep stuck at %v before burst end %v", s,
				c.ReconcileThrough, b.to)
		}
	}
	for key, width := range rt.narrowed {
		t.Errorf("%s pass %d still narrowed to %v after sparse reads", key.stream, key.pass,
			width)
	}
	for _, err := range lastErrs {
		if errors.Is(err, ErrBudget) {
			t.Errorf("poll still saturated after 120 polls: %v", err)
		}
	}
}

func TestReconcileWidenKeepsDenseHintAndForgetsItAtFullWidth(t *testing.T) {
	rt, err := NewRuntime(&rateReader{}, &Store{}, testScope(), Options{WarehouseID: "test",
		Streams: []Stream{Plans}})
	if err != nil {
		t.Fatal(err)
	}
	o := rt.options
	sp := &streamPoll{stream: Plans}
	key := narrowKey{Plans, deepPass}
	w := Window{scheduleEpoch, scheduleEpoch.Add(5 * time.Minute)}
	rt.narrowed[key] = 5 * time.Minute
	rt.widen(sp, deepPass, w, o.MaxRows/4)
	if got := rt.narrowed[key]; got != 5*time.Minute {
		t.Fatalf("dense read at the quarter boundary moved hint to %v", got)
	}
	rt.widen(sp, deepPass, w, o.MaxRows/4-1)
	if got := rt.narrowed[key]; got != 10*time.Minute {
		t.Fatalf("sparse read hint %v, want doubled to 10m", got)
	}
	rt.widen(sp, deepPass, Window{w.From, w.From.Add(10 * time.Minute)}, 0)
	if _, ok := rt.narrowed[key]; ok {
		t.Fatalf("hint %v kept at the pass width %v", rt.narrowed[key], o.Window)
	}
	rt.widen(sp, shortPass, w, 0)
	if len(rt.narrowed) != 0 {
		t.Fatalf("widen created a hint for an un-narrowed pass: %v", rt.narrowed)
	}
}
