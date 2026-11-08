package runway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-02 review: a tick whose trends are unreadable measured and
// resolved nothing, so it leaves the stale marks (and their logs) alone.
//
// No concurrency test: one monitor goroutine ticks per runtime.

// probeFailer fails the probes in fail; the test changes fail between
// ticks (one goroutine, so no lock).
type probeFailer struct {
	inner ProbeRunner
	fail  map[probes.ID]bool
}

func (f *probeFailer) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	if f.fail[id] {
		return probes.Result{ProbeID: id, Status: probes.StatusError,
			Reason: "statement_timeout", ObservedAt: time.Now()}
	}
	return f.inner.Run(ctx, id, a)
}

func (f *probeFailer) RunBackground(ctx context.Context, id probes.ID,
	a probes.Args) probes.Result {
	if f.fail[id] {
		return f.Run(ctx, id, a)
	}
	return f.inner.RunBackground(ctx, id, a)
}

// Unreadable trends hold nothing and return nil, so the monitor leaves
// its stale marks (and their logging) alone that tick.
func TestHolds_UnreadableTrendsReturnNil(t *testing.T) {
	in := EvalInput{TrendsOK: false, SlotsOK: true, Slots: []probes.Slot{{Name: "cdc"}}}
	if h := Holds(in, testOptions()); h != nil {
		t.Fatalf("unreadable trends held %v, want nil", h)
	}
}

// A tick with unreadable trends neither releases nor re-logs the stale
// mark: becoming stale and being measured again are each logged once.
func TestMonitorTick_UnreadableTrendsKeepTheStaleMarks(t *testing.T) {
	pool, ctx := livePool(t)
	resetXID(t, ctx, pool)
	seedXIDTrend(t, ctx, pool, 7*time.Hour)
	openXIDFinding(t, ctx, pool)
	run := &probeFailer{inner: pastGateXID(liveRunner(pool)), fail: map[probes.ID]bool{}}
	m := newTestMonitor(t, pool, run, nil)
	var mu sync.Mutex
	var logs []string
	m.logFn = func(_, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(msg, args...))
	}
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range logs {
			if strings.Contains(l, sub) && strings.Contains(l, " xid ") {
				n++
			}
		}
		return n
	}
	for _, trendsFail := range []bool{false, true, false} {
		run.fail[probes.RunwayTrendsProbe] = trendsFail
		_, _ = m.Tick(ctx) // the failed probes are named; the logs are under test
	}
	if n := count("no longer held stale"); n != 0 {
		t.Fatalf("an unreadable-trends tick released the stale mark %d times (%v)", n, logs)
	}
	if n := count("held open, marked stale"); n != 1 {
		t.Fatalf("%d stale logs, want 1 (%v)", n, logs)
	}
	seedXIDTrend(t, ctx, pool, 0)
	_, _ = m.Tick(ctx)
	if n := count("no longer held stale"); n != 1 {
		t.Fatalf("%d release logs after measuring again, want 1 (%v)", n, logs)
	}
}
