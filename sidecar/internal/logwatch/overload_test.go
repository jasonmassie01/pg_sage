package logwatch

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-06: a drain that lost lines emits log_watcher_overloaded with the
// counts, the totals feed /metrics, and the fanout hands critical signals
// to the log fast path.

func findSignal(signals []*rca.Signal, id string) *rca.Signal {
	for _, s := range signals {
		if s != nil && s.ID == id {
			return s
		}
	}
	return nil
}

func countSignals(signals []*rca.Signal, id string) int {
	n := 0
	for _, s := range signals {
		if s != nil && s.ID == id {
			n++
		}
	}
	return n
}

func intMetricOf(t *testing.T, s *rca.Signal, key string) int {
	t.Helper()
	v, ok := s.Metrics[key].(int)
	if !ok {
		t.Fatalf("metric %q = %#v (%T), want int", key, s.Metrics[key], s.Metrics[key])
	}
	return v
}

// Boundary: 20,000 noise ERROR lines and one PANIC keep the PANIC signal,
// report dropped_lines >= 10000 and emit the overload signal.
func TestDrainStormKeepsPanicAndEmitsOverload(t *testing.T) {
	fanout, path := startTestFanout(t, 10000)
	sub := fanout.Subscribe("db")
	panicLine := jsonRecord(t, "PANIC", "XX000", "could not write WAL", 1)
	appendLogBytes(t, path, noiseStorm(t, 20000, 10, panicLine))

	fanout.DrainSource()
	signals := sub.Drain()
	if findSignal(signals, "log_panic_server_crash") == nil {
		t.Fatalf("PANIC signal lost in the storm: %d signals", len(signals))
	}
	over := findSignal(signals, rca.SignalLogWatcherOverloaded)
	if over == nil {
		t.Fatal("no log_watcher_overloaded signal after a queue overflow")
	}
	if over.Severity != "warning" {
		t.Errorf("overload severity = %q, want warning", over.Severity)
	}
	if got := intMetricOf(t, over, "dropped_lines"); got != 10001 {
		t.Errorf("dropped_lines = %d, want 10001 (>= 10000)", got)
	}
	if got := intMetricOf(t, over, "queue_overflow_lines"); got != 10001 {
		t.Errorf("queue_overflow_lines = %d, want 10001", got)
	}
	if got := intMetricOf(t, over, "classifier_cap_lines"); got != 0 {
		t.Errorf("classifier_cap_lines = %d, want 0", got)
	}
	if over.FiredAt.IsZero() {
		t.Error("overload signal has no FiredAt")
	}
	if _, ok := over.Metrics["database"]; ok {
		t.Error("overload signal must be cluster-wide (no database)")
	}
	got := fanout.DroppedLines()
	if got.QueueOverflow != 10001 || got.ClassifierCap != 0 {
		t.Fatalf("DroppedLines = %+v, want queue 10001, cap 0", got)
	}
}

// The classifier cap's skipped lines are reported too, by reason.
func TestDrainClassifierCapEmitsOverload(t *testing.T) {
	fanout, path := startTestFanout(t, 100)
	sub := fanout.Subscribe("db")
	disk := jsonRecord(t, "ERROR", "53100", "could not extend file: No space left", 9)
	appendLogBytes(t, path, noiseStorm(t, 150, 150, nil))
	appendLogBytes(t, path, append(disk, '\n'))

	fanout.DrainSource()
	signals := sub.Drain()
	if findSignal(signals, "log_disk_full") == nil {
		t.Fatal("disk-full past the classifier cap was not classified")
	}
	over := findSignal(signals, rca.SignalLogWatcherOverloaded)
	if over == nil {
		t.Fatal("no overload signal after the classifier cap dropped lines")
	}
	if got := intMetricOf(t, over, "classifier_cap_lines"); got != 50 {
		t.Errorf("classifier_cap_lines = %d, want 50", got)
	}
	if got := intMetricOf(t, over, "dropped_lines"); got != 50 {
		t.Errorf("dropped_lines = %d, want 50", got)
	}
	if got := fanout.DroppedLines(); got.ClassifierCap != 50 || got.QueueOverflow != 0 {
		t.Fatalf("DroppedLines = %+v, want cap 50", got)
	}
}

// A drain that lost nothing emits no overload signal; totals accumulate
// across drains and never reset.
func TestDrainWithoutDropsHasNoOverloadAndTotalsAccumulate(t *testing.T) {
	fanout, path := startTestFanout(t, 0)
	sub := fanout.Subscribe("db")
	appendLogBytes(t, path, noiseStorm(t, 10, -1, nil))
	fanout.DrainSource()
	if s := findSignal(sub.Drain(), rca.SignalLogWatcherOverloaded); s != nil {
		t.Fatalf("overload signal without drops: %+v", s)
	}
	if got := fanout.DroppedLines(); got != (DropCounts{}) {
		t.Fatalf("DroppedLines = %+v, want zero", got)
	}
	for round := 0; round < 2; round++ {
		appendLogBytes(t, path, noiseStorm(t, maxTailerQueueSize+7, -1, nil))
		fanout.DrainSource()
	}
	if got := fanout.DroppedLines(); got.QueueOverflow != 14 {
		t.Fatalf("cumulative queue drops = %d, want 14", got.QueueOverflow)
	}
	if n := countSignals(sub.Drain(), rca.SignalLogWatcherOverloaded); n != 2 {
		t.Fatalf("overload signals = %d, want one per lossy drain (2)", n)
	}
}

// Nil/empty: a watcher that never started reports no drops.
func TestDroppedLinesBeforeStart(t *testing.T) {
	fw := NewFileWatcher(config.LogWatchConfig{}, nopLog)
	if got := fw.DroppedLines(); got != (DropCounts{}) {
		t.Fatalf("DroppedLines before Start = %+v", got)
	}
	if fw.Drain() != nil {
		t.Fatal("Drain before Start returned signals")
	}
}

func lockTimeoutLine(t *testing.T, pid int) []byte {
	return jsonRecord(t, "ERROR", "55P03",
		"canceling statement due to lock timeout", pid)
}

// DrainCritical takes only critical signals, in order, and leaves the
// rest for the analyzer cycle; other subscribers are unaffected.
func TestFanoutDrainCriticalSplitsBuffer(t *testing.T) {
	fanout, path := startTestFanout(t, 0)
	fast := fanout.Subscribe("fast")
	other := fanout.Subscribe("other")
	var data bytes.Buffer
	data.Write(lockTimeoutLine(t, 1))
	data.WriteByte('\n')
	appendLogBytes(t, path, data.Bytes())
	appendOOMBatch(t, path, 0, 2)

	fanout.DrainSource()
	critical := fast.DrainCritical()
	requireSignalDatabases(t, critical, "db-0", "db-1")
	for _, s := range critical {
		if s.Severity != "critical" {
			t.Fatalf("DrainCritical returned %s (%s)", s.ID, s.Severity)
		}
	}
	if again := fast.DrainCritical(); len(again) != 0 {
		t.Fatalf("second DrainCritical = %d signals, want 0", len(again))
	}
	rest := fast.Drain()
	if len(rest) != 1 || rest[0].ID != "log_lock_timeout" {
		t.Fatalf("remaining = %+v, want the lock timeout only", rest)
	}
	if n := len(other.Drain()); n != 3 {
		t.Fatalf("other subscriber got %d signals, want 3", n)
	}
}

// Nil/empty: an empty buffer and an unknown subscriber drain nothing.
func TestFanoutDrainCriticalEmpty(t *testing.T) {
	fanout, _ := startTestFanout(t, 0)
	sub := fanout.Subscribe("db")
	if got := sub.DrainCritical(); len(got) != 0 {
		t.Fatalf("empty DrainCritical = %d", len(got))
	}
	ghost := &FanoutSubscriber{fanout: fanout, id: "ghost"}
	if got := ghost.DrainCritical(); len(got) != 0 {
		t.Fatalf("unknown subscriber DrainCritical = %d", len(got))
	}
}

// Concurrent DrainSource, DrainCritical and Drain lose no signal and
// deliver none twice.
func TestFanoutDrainCriticalConcurrent(t *testing.T) {
	fanout, path := startTestFanout(t, 0)
	sub := fanout.Subscribe("db")
	const batches = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	record := func(signals []*rca.Signal) {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range signals {
			seen[s.ID+"/"+s.Metrics["database"].(string)]++
		}
	}
	stop := make(chan struct{})
	for _, drain := range []func() []*rca.Signal{sub.DrainCritical, sub.Drain} {
		wg.Add(1)
		go func(drain func() []*rca.Signal) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					record(drain())
				}
			}
		}(drain)
	}
	for i := 0; i < batches; i++ {
		appendOOMBatch(t, path, i, 1)
		fanout.DrainSource()
	}
	close(stop)
	wg.Wait()
	record(sub.Drain())
	if len(seen) != batches {
		t.Fatalf("distinct signals = %d, want %d", len(seen), batches)
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("signal %s delivered %d times", k, n)
		}
	}
}

// Integration across packages: a disk-full line reaches the RCA engine
// through DrainCritical and opens a critical incident without a cycle.
func TestDiskFullLineReachesEngineFastPath(t *testing.T) {
	fanout, path := startTestFanout(t, 10000)
	sub := fanout.Subscribe("db")
	disk := jsonRecord(t, "ERROR", "53100",
		"could not extend file \"base/1/2\": No space left on device", 7)
	appendLogBytes(t, path, append(disk, '\n'))
	fanout.DrainSource()

	eng := rca.NewEngine(&config.RCAConfig{ResolutionCycles: 2,
		DedupWindowMinutes: 30, StaleAfterHours: 24}, nopLog)
	eng.WithLogReplayCutoff(time.Time{})
	eng.WithDatabaseName("db")
	touched := eng.ObserveLogSignals(context.Background(), sub.DrainCritical())
	if len(touched) != 1 {
		t.Fatalf("touched incidents = %d, want 1", len(touched))
	}
	inc := touched[0]
	if inc.Severity != "critical" || len(inc.SignalIDs) != 1 ||
		inc.SignalIDs[0] != "log_disk_full" {
		t.Fatalf("incident = %+v, want critical log_disk_full", inc)
	}
	if inc.DatabaseName != "db" {
		t.Fatalf("database = %q, want db", inc.DatabaseName)
	}
	if n := len(eng.ActiveIncidents()); n != 1 {
		t.Fatalf("active incidents = %d, want 1", n)
	}
}
