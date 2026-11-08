package rca

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// rca-06 against a real sage.incidents store: the log fast path persists
// a disk-full incident and notifies once without an analyzer cycle.

// Happy path (CHECK-16): with the analyzer cycle at 600 s and never run,
// a disk-full line opens a persisted incident and exactly one
// incident_detected within two fast-path ticks.
func TestLogFastPathPersistsDiskFullWithinTwoTicks(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	src := &fakeCriticalSource{}
	const interval = 5 * time.Second
	lt := NewLogSignalTicker(eng, pool, src, interval, func(string, string, ...any) {})
	clock := newFakeClock()
	lt.newTicker = clock.newTicker
	results := make(chan error, 4)
	real := lt.tick
	lt.tick = func(ctx context.Context) error {
		err := real(ctx)
		results <- err
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { lt.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-clock.ready

	src.push(diskFullSignal())
	ticks := 0
	for ticks < 2 && len(incidentRows(t, ctx, pool, db)) == 0 {
		clock.Advance(interval)
		ticks++
		if err := awaitTick(t, results); err != nil {
			t.Fatalf("tick %d: %v", ticks, err)
		}
	}
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 || rows[0].resolvedAt != nil {
		t.Fatalf("after %d ticks rows = %+v, want one open incident", ticks, rows)
	}
	clock.Advance(interval) // a quiet tick must not notify again
	if err := awaitTick(t, results); err != nil {
		t.Fatalf("quiet tick: %v", err)
	}
	detected := rec.byType("incident_detected")
	if len(detected) != 1 {
		t.Fatalf("incident_detected = %d, want 1", len(detected))
	}
	if detected[0].Data["incident_id"] != rows[0].id ||
		detected[0].Severity != "critical" {
		t.Fatalf("event = %+v, want critical for %s", detected[0], rows[0].id)
	}
}

func awaitTick(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("fast-path tick did not finish")
		return nil
	}
}

// Concurrency (CHECK-18): the fast path and an analyzer cycle that both
// see a disk-full line give one incident and one notification.
func TestLogFastPathAndCycleRaceNotifyOnce(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprintf("round%d", i), func(t *testing.T) {
			eng, db := lifecycleEngine(t, pool)
			rec := &recordingDispatcher{}
			eng.WithDispatcher(rec)
			eng.SetLogSource(&mockLogSource{signals: []*Signal{diskFullSignal()}})
			src := &fakeCriticalSource{}
			src.push(diskFullSignal())
			lt := NewLogSignalTicker(eng, pool, src, time.Second,
				func(string, string, ...any) {})
			raceFastPathAndCycle(t, ctx, eng, lt)
			if src.callCount() != 1 {
				t.Fatalf("fast path drained %d times, want 1", src.callCount())
			}
			if err := eng.PersistIncidents(ctx, pool); err != nil {
				t.Fatalf("final persist: %v", err)
			}
			if rows := incidentRows(t, ctx, pool, db); len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if n := len(rec.byType("incident_detected")); n != 1 {
				t.Fatalf("incident_detected = %d, want exactly 1", n)
			}
		})
	}
}

func raceFastPathAndCycle(
	t *testing.T, ctx context.Context, eng *Engine, lt *LogSignalTicker,
) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- lt.Tick(ctx)
	}()
	go func() {
		defer wg.Done()
		<-start
		if err := eng.Hydrate(ctx, lt.pool); err != nil {
			errs <- err
			return
		}
		eng.AnalyzeContext(ctx, quietSnapshot(), nil, testConfig(), nil)
		errs <- eng.PersistIncidents(ctx, lt.pool)
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("racing path: %v", err)
		}
	}
}
