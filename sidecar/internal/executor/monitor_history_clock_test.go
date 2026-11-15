package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// No concurrent access tests: shiftedObserver and historyShift keep no
// state; each call reads its own clocks.

// windowRecorder is a queryObserver that records the window it was asked
// for and answers with one measurement.
type windowRecorder struct {
	calls    int
	from, to time.Time
}

func (w *windowRecorder) observe(_ context.Context, ids []int64, from, to time.Time) (
	map[int64]verify.Measurement, error) {
	w.calls++
	w.from, w.to = from, to
	out := map[int64]verify.Measurement{}
	for _, id := range ids {
		out[id] = verify.Measurement{Samples: 42}
	}
	return out, nil
}

func fixedShift(d time.Duration, err error) func(context.Context) (time.Duration, error) {
	return func(context.Context) (time.Duration, error) { return d, err }
}

// A meta database whose clock leads the monitored one's stamped the
// samples later: the window moves onto its clock, both ends.
func TestShiftedObserver_MovesTheWindowOntoTheHistoryClock(t *testing.T) {
	rec := &windowRecorder{}
	from := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Minute)

	got, err := shiftedObserver(rec.observe, fixedShift(10*time.Minute, nil))(
		context.Background(), []int64{7}, from, to)

	if err != nil || got[7].Samples != 42 {
		t.Fatalf("observe = %+v, %v; want the observer's measurement", got, err)
	}
	if !rec.from.Equal(from.Add(10*time.Minute)) || !rec.to.Equal(to.Add(10*time.Minute)) {
		t.Fatalf("window = %s..%s, want %s..%s", rec.from, rec.to,
			from.Add(10*time.Minute), to.Add(10*time.Minute))
	}
}

// A lagging history clock moves the window back; no lead leaves it alone.
func TestShiftedObserver_LagAndZeroShift(t *testing.T) {
	from := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for _, shift := range []time.Duration{-3 * time.Minute, 0} {
		rec := &windowRecorder{}
		if _, err := shiftedObserver(rec.observe, fixedShift(shift, nil))(
			context.Background(), []int64{1}, from, to); err != nil {
			t.Fatalf("shift %s: %v", shift, err)
		}
		if !rec.from.Equal(from.Add(shift)) || !rec.to.Equal(to.Add(shift)) {
			t.Fatalf("shift %s: window = %s..%s", shift, rec.from, rec.to)
		}
	}
}

// A failed clock read is a failed sample read: nothing is read on a clock
// the samples were not stamped by, and the error says which read failed.
func TestShiftedObserver_FailedShiftIsAFailedRead(t *testing.T) {
	rec := &windowRecorder{}
	cause := errors.New("meta database down")

	got, err := shiftedObserver(rec.observe, fixedShift(0, cause))(
		context.Background(), []int64{1}, time.Now(), time.Now())

	if err == nil || !errors.Is(err, cause) || !strings.Contains(err.Error(), "history clock") {
		t.Fatalf("error = %v, want the wrapped clock failure", err)
	}
	if got != nil || rec.calls != 0 {
		t.Fatalf("observed %d times (%v), want no sample read", rec.calls, got)
	}
}

// Without a pool the history is no meta database's: no shift, no read.
func TestHistoryShift_NilPoolIsUnshifted(t *testing.T) {
	d, err := historyShift(context.Background(), nil)
	if err != nil || d != 0 {
		t.Fatalf("historyShift(nil) = %s, %v; want 0, nil", d, err)
	}
}
