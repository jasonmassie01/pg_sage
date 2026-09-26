package providerobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

type window struct{ from, to time.Time }

// windowAPI records every log window requested and fails according to a
// caller-supplied rule.
type windowAPI struct {
	mu      sync.Mutex
	now     time.Time
	windows []window
	fail    func(from, to time.Time) error
}

func (w *windowAPI) Metrics(context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return scrape(w.now, 100), nil
}

func (w *windowAPI) Logs(_ context.Context, _ string, from, to time.Time) (
	[]logwatch.LogEntry, error,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.windows = append(w.windows, window{from, to})
	if w.fail != nil {
		return nil, w.fail(from, to)
	}
	return nil, nil
}

func (w *windowAPI) last() window {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.windows[len(w.windows)-1]
}

// G1-B10: a long provider outage must never grow the request window past
// the provider's 24h limit (which would wedge ingestion forever), and the
// first successful poll after recovery must advance the cursor.
func TestPollLogs_OutageKeepsWindowBoundedAndRecovers(t *testing.T) {
	start := time.Date(2026, 9, 26, 12, 0, 30, 0, time.UTC)
	api := &windowAPI{now: start}
	r, err := NewRuntime(api, "postgres",
		func(context.Context, []logwatch.LogEntry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.pollLogs(context.Background(), start); err != nil {
		t.Fatalf("initial poll: %v", err)
	}
	api.fail = func(time.Time, time.Time) error { return errors.New("provider down") }
	now := start
	for i := 0; i < 30*60; i++ { // 30 hours of one-minute polls
		now = now.Add(time.Minute)
		_ = r.pollLogs(context.Background(), now)
		w := api.last()
		if span := w.to.Sub(w.from); span <= 0 || span > maxLogWindow {
			t.Fatalf("poll %d requested window %v, want (0, %v]", i, span, maxLogWindow)
		}
	}
	api.fail = nil
	now = now.Add(time.Minute)
	if err := r.pollLogs(context.Background(), now); err == nil {
		t.Fatal("recovery after exceeding max lag must report the skipped gap")
	}
	until := now.Truncate(time.Minute).Add(-time.Minute)
	if lag := until.Sub(r.through); lag > maxLogLag {
		t.Fatalf("cursor lag after recovery = %v, want <= %v", lag, maxLogLag)
	}
	before := r.through
	now = now.Add(time.Minute)
	if err := r.pollLogs(context.Background(), now); err != nil {
		t.Fatalf("steady-state poll after recovery: %v", err)
	}
	if !r.through.After(before) {
		t.Fatalf("cursor did not advance after recovery: %v -> %v", before, r.through)
	}
}

// G1-B10: a saturated window shrinks; if even the minimum window is
// saturated the cursor still advances (reporting the loss) instead of
// retrying the same saturated window forever.
func TestPollLogs_SaturationDoesNotWedgeCursor(t *testing.T) {
	start := time.Date(2026, 9, 26, 12, 0, 30, 0, time.UTC)
	api := &windowAPI{now: start}
	r, _ := NewRuntime(api, "postgres",
		func(context.Context, []logwatch.LogEntry) error { return nil })
	if err := r.pollLogs(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	api.fail = func(time.Time, time.Time) error { return ErrLogWindowSaturated }
	now := start
	var cursors []time.Time
	for i := 0; i < 5; i++ {
		now = now.Add(time.Minute)
		err := r.pollLogs(context.Background(), now)
		if !errors.Is(err, ErrLogWindowSaturated) {
			t.Fatalf("poll %d error = %v, want saturation reported", i, err)
		}
		cursors = append(cursors, r.through)
	}
	for i := 1; i < len(cursors); i++ {
		if !cursors[i].After(cursors[i-1]) {
			t.Fatalf("cursor wedged under saturation: %v", cursors)
		}
	}
}
