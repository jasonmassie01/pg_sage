package rca

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// rca-06: critical log signals open incidents on the log fast path
// instead of waiting for the analyzer cycle.

type fakeCriticalSource struct {
	mu      sync.Mutex
	signals []*Signal
	calls   int
}

func (f *fakeCriticalSource) DrainCritical() []*Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := f.signals
	f.signals = nil
	return out
}

func (f *fakeCriticalSource) push(s ...*Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, s...)
}

func (f *fakeCriticalSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func diskFullSignal() *Signal {
	return &Signal{ID: "log_disk_full", FiredAt: time.Now(), Severity: "critical",
		Metrics: map[string]any{"error_level": "ERROR", "sql_state": "53100",
			"message": "could not extend file: No space left on device"}}
}

func lockTimeoutSignal() *Signal {
	return &Signal{ID: "log_lock_timeout", FiredAt: time.Now(), Severity: "warning",
		Metrics: map[string]any{"sql_state": "55P03", "database": "app"}}
}

func TestObserveLogSignalsNilAndEmpty(t *testing.T) {
	var nilEng *Engine
	if got := nilEng.ObserveLogSignals(context.Background(),
		[]*Signal{diskFullSignal()}); got != nil {
		t.Fatalf("nil engine = %v, want nil", got)
	}
	eng := testEngine()
	for name, in := range map[string][]*Signal{
		"nil": nil, "empty": {}, "nil entry": {nil},
		"warning only": {lockTimeoutSignal()},
	} {
		if got := eng.ObserveLogSignals(context.Background(), in); len(got) != 0 {
			t.Fatalf("%s: touched %d incidents, want 0", name, len(got))
		}
	}
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Fatalf("active incidents = %d, want 0", n)
	}
	if len(eng.fastFired) != 0 {
		t.Fatalf("fastFired = %v, want empty", eng.fastFired)
	}
}

func TestObserveLogSignalsOpensCriticalIncident(t *testing.T) {
	eng := testEngine()
	eng.WithDatabaseName("orders")
	got := eng.ObserveLogSignals(context.Background(),
		[]*Signal{diskFullSignal(), lockTimeoutSignal()})
	if len(got) != 1 {
		t.Fatalf("touched = %d, want 1 (the disk-full incident)", len(got))
	}
	inc := got[0]
	if inc.Severity != "critical" || inc.DatabaseName != "orders" ||
		len(inc.SignalIDs) != 1 || inc.SignalIDs[0] != "log_disk_full" {
		t.Fatalf("incident = %+v", inc)
	}
	if !strings.Contains(inc.RootCause, "Disk full") {
		t.Fatalf("root cause = %q", inc.RootCause)
	}
	if inc.ID == "" || inc.OccurrenceCount != 1 {
		t.Fatalf("id=%q occurrences=%d", inc.ID, inc.OccurrenceCount)
	}
	if !eng.fastFired["log_disk_full"] || eng.fastFired["log_lock_timeout"] {
		t.Fatalf("fastFired = %v, want only log_disk_full", eng.fastFired)
	}
	if !eng.trackFor(inc.ID).notifyDetected {
		t.Fatal("a new fast-path incident must be queued for incident_detected")
	}
}

// Occurrences count once per analyzer-cycle window, as the cycle did
// before the fast path existed.
func TestObserveLogSignalsCountsOncePerCycle(t *testing.T) {
	eng := testEngine()
	ctx := context.Background()
	first := eng.ObserveLogSignals(ctx, []*Signal{diskFullSignal()})
	again := eng.ObserveLogSignals(ctx, []*Signal{diskFullSignal()})
	if len(first) != 1 || len(again) != 1 || first[0].ID != again[0].ID {
		t.Fatalf("repeat sighting opened a second incident: %v / %v", first, again)
	}
	if again[0].OccurrenceCount != 1 {
		t.Fatalf("occurrences in one window = %d, want 1", again[0].OccurrenceCount)
	}
	eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)
	third := eng.ObserveLogSignals(ctx, []*Signal{diskFullSignal()})
	if len(third) != 1 || third[0].ID != first[0].ID {
		t.Fatalf("after a cycle: %v", third)
	}
	if third[0].OccurrenceCount != 2 {
		t.Fatalf("occurrences after a cycle = %d, want 2", third[0].OccurrenceCount)
	}
}

// The fast path applies the same replay and database filters as the cycle.
func TestObserveLogSignalsFiltersReplayAndForeign(t *testing.T) {
	eng := testEngine()
	eng.WithLogDatabase("app")
	old := diskFullSignal()
	old.FiredAt = time.Now().Add(-time.Hour)
	foreign := diskFullSignal()
	foreign.Metrics["database"] = "other"
	if got := eng.ObserveLogSignals(context.Background(),
		[]*Signal{old, foreign}); len(got) != 0 {
		t.Fatalf("replayed/foreign signals opened %d incidents", len(got))
	}
	mine := diskFullSignal()
	mine.Metrics["database"] = "app"
	if got := eng.ObserveLogSignals(context.Background(),
		[]*Signal{mine}); len(got) != 1 {
		t.Fatalf("own-database signal touched %d incidents, want 1", len(got))
	}
}

// State transition: a fast-path incident counts as firing in the next
// analyzer cycle; clear cycles do not resolve a log incident (rca-07), the
// quiet period (rca.log_quiet_minutes, default the 30 min dedup window)
// does.
func TestFastPathLogIncidentAutoResolves(t *testing.T) {
	eng := testEngine() // resolution_cycles = 2
	now := time.Now()
	eng.now = func() time.Time { return now }
	cfg := testConfig()
	for i := 0; i < 4; i++ { // burn the grace period on quiet cycles
		eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	}
	opened := eng.ObserveLogSignals(context.Background(), []*Signal{diskFullSignal()})
	if len(opened) != 1 {
		t.Fatalf("opened = %d", len(opened))
	}
	id := opened[0].ID
	eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil) // fast-fired
	if !isOpen(eng, id) {
		t.Fatal("resolved in the cycle that consumed the fast-path sighting")
	}
	eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil) // clear 1
	if !isOpen(eng, id) {
		t.Fatal("resolved after 1 clear cycle, want 2")
	}
	eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil) // clear 2
	if !isOpen(eng, id) {
		t.Fatal("log incident resolved by clear cycles, want the quiet period")
	}
	now = now.Add(31 * time.Minute)
	eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	if isOpen(eng, id) {
		t.Fatal("still open after the quiet period")
	}
	for _, inc := range eng.incidents {
		if inc.ID == id && inc.ResolvedBy != ResolvedByAuto {
			t.Fatalf("resolved_by = %q, want %q", inc.ResolvedBy, ResolvedByAuto)
		}
	}
}

func isOpen(eng *Engine, id string) bool {
	for _, inc := range eng.ActiveIncidents() {
		if inc.ID == id {
			return true
		}
	}
	return false
}

// The overload signal is a warning: it waits for the cycle, which maps it
// to an incident that says how many lines were lost.
func TestOverloadSignalMapsToIncidentOnCycle(t *testing.T) {
	over := &Signal{ID: SignalLogWatcherOverloaded, FiredAt: time.Now(),
		Severity: "warning", Metrics: map[string]any{"dropped_lines": 10001,
			"queue_overflow_lines": 10001, "classifier_cap_lines": 0}}
	eng := testEngine()
	if got := eng.ObserveLogSignals(context.Background(),
		[]*Signal{over}); len(got) != 0 {
		t.Fatalf("overload took the fast path: %v", got)
	}
	eng.SetLogSource(&mockLogSource{signals: []*Signal{over}})
	incidents := eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)
	inc := requireOne(t, incidents)
	if inc.Severity != "warning" || inc.SignalIDs[0] != SignalLogWatcherOverloaded {
		t.Fatalf("incident = %+v", inc)
	}
	if !strings.Contains(inc.RootCause, "10001") ||
		!strings.Contains(inc.RootCause, "dropped") {
		t.Fatalf("root cause %q does not report the dropped lines", inc.RootCause)
	}
	if inc.ActionRisk != "safe" || inc.RecommendedSQL != "" {
		t.Fatalf("overload must not propose SQL: risk=%q sql=%q",
			inc.ActionRisk, inc.RecommendedSQL)
	}
	if len(inc.CausalChain) < 2 ||
		!strings.Contains(inc.CausalChain[0].Evidence, "queue_overflow=10001") {
		t.Fatalf("chain = %+v", inc.CausalChain)
	}
}

func newTestLogTicker(
	src CriticalLogSource, interval time.Duration,
) (*LogSignalTicker, *logSink) {
	sink := &logSink{}
	lt := NewLogSignalTicker(testEngine(), nil, src, interval, sink.log)
	lt.hydrate = func(context.Context) error { return nil }
	lt.persist = func(context.Context) error { return nil }
	return lt, sink
}

// Nil/empty: no log source means the ticker never starts; 0 disables it.
func TestLogSignalTickerDisabled(t *testing.T) {
	for name, tc := range map[string]struct {
		src      CriticalLogSource
		interval time.Duration
	}{
		"no source": {nil, 5 * time.Second},
		"zero":      {&fakeCriticalSource{}, 0},
		"negative":  {&fakeCriticalSource{}, -time.Second},
	} {
		lt, _ := newTestLogTicker(tc.src, tc.interval)
		started := false
		lt.newTicker = func(time.Duration) (<-chan time.Time, func()) {
			started = true
			return nil, func() {}
		}
		done := make(chan struct{})
		go func() { lt.Run(context.Background()); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Run did not return", name)
		}
		if started {
			t.Fatalf("%s: ticker started", name)
		}
	}
}

func TestLogSignalTickerTickOpensAndPersists(t *testing.T) {
	src := &fakeCriticalSource{}
	lt, _ := newTestLogTicker(src, time.Second)
	persisted := 0
	lt.persist = func(context.Context) error { persisted++; return nil }
	if err := lt.Tick(context.Background()); err != nil {
		t.Fatalf("quiet tick: %v", err)
	}
	if persisted != 0 {
		t.Fatal("a tick without critical signals persisted")
	}
	src.push(diskFullSignal())
	if err := lt.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if persisted != 1 || len(lt.eng.ActiveIncidents()) != 1 {
		t.Fatalf("persisted=%d active=%d, want 1/1", persisted,
			len(lt.eng.ActiveIncidents()))
	}
}

// Error propagation: a failed hydrate leaves the signals in the source for
// the next tick or the analyzer cycle.
func TestLogSignalTickerHydrateFailureKeepsSignals(t *testing.T) {
	src := &fakeCriticalSource{}
	src.push(diskFullSignal())
	lt, _ := newTestLogTicker(src, time.Second)
	lt.hydrate = func(context.Context) error { return errors.New("connection refused") }
	err := lt.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hydrate incident state") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want hydrate context", err)
	}
	if src.callCount() != 0 || len(src.signals) != 1 {
		t.Fatalf("signals drained despite failed hydrate (calls=%d)", src.callCount())
	}
}

// Error propagation: a failed persist is retried on the next tick even
// when no new signal arrives, so the notification is not left to the cycle.
func TestLogSignalTickerRetriesFailedPersist(t *testing.T) {
	src := &fakeCriticalSource{}
	src.push(diskFullSignal())
	lt, _ := newTestLogTicker(src, time.Second)
	fail := true
	calls := 0
	lt.persist = func(context.Context) error {
		calls++
		if fail {
			return errors.New("permission denied for table incidents")
		}
		return nil
	}
	err := lt.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "persist incidents") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want persist context", err)
	}
	fail = false
	if err := lt.Tick(context.Background()); err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if calls != 2 {
		t.Fatalf("persist calls = %d, want 2 (retry)", calls)
	}
	if err := lt.Tick(context.Background()); err != nil || calls != 2 {
		t.Fatalf("after success a quiet tick persisted again (calls=%d, err=%v)",
			calls, err)
	}
}

// Run logs a failed tick at WARN and keeps ticking.
func TestLogSignalTickerRunSurvivesTickErrors(t *testing.T) {
	clock := newFakeClock()
	lt, sink := newTestLogTicker(&fakeCriticalSource{}, time.Second)
	lt.newTicker = clock.newTicker
	ticks := make(chan struct{}, 4)
	lt.tick = func(context.Context) error {
		ticks <- struct{}{}
		return errors.New("boom")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { lt.Run(ctx); close(done) }()
	<-clock.ready
	if clock.period != time.Second {
		t.Fatalf("ticker period = %s, want 1s", clock.period)
	}
	clock.Advance(time.Second)
	waitTick(t, ticks, true)
	clock.Advance(time.Second)
	waitTick(t, ticks, true)
	cancel()
	<-done
	if !sink.contains("WARN rca: log fast path: boom") {
		t.Fatalf("tick error not logged: %v", sink.lines)
	}
	if !clock.isStopped() {
		t.Fatal("ticker not stopped on exit")
	}
	if lt.Interval() != time.Second {
		t.Fatalf("Interval() = %s", lt.Interval())
	}
}

// Boundary: the watcher emits one overload signal per lossy drain (about
// every second), so the cycle's incident must report their sum, not only
// the last drain's loss.
func TestOverloadIncidentSumsTheCycleDrains(t *testing.T) {
	now := time.Now()
	drain := func(at time.Time, queue, capped int) *Signal {
		return &Signal{ID: SignalLogWatcherOverloaded, FiredAt: at,
			Severity: "warning", Metrics: map[string]any{
				"dropped_lines": queue + capped, "queue_overflow_lines": queue,
				"classifier_cap_lines": capped}}
	}
	eng := testEngine()
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		drain(now.Add(-2*time.Second), 3000, 0),
		drain(now, 4000, 25)}})
	inc := requireOne(t, eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil))
	if !strings.Contains(inc.RootCause, "7025 log lines dropped") {
		t.Fatalf("root cause %q, want the cycle total 7025", inc.RootCause)
	}
	if ev := inc.CausalChain[0].Evidence; ev != "queue_overflow=7000 classifier_cap=25" {
		t.Fatalf("evidence = %q", ev)
	}
	if !inc.LastDetectedAt.Equal(now) && !inc.DetectedAt.Equal(now) {
		t.Fatalf("detected %v / last %v, want the latest drain %v",
			inc.DetectedAt, inc.LastDetectedAt, now)
	}
	if sumOverloadSignals([]*Signal{nil, diskFullSignal()}) != nil {
		t.Fatal("no overload signal must sum to nil")
	}
}
