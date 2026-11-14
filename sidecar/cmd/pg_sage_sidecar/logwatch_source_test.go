package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/rca"
)

// fakeAfter is a fake clock for the log-source retry: every wait is
// recorded and fires only when the test says so.
type fakeAfter struct {
	mu      sync.Mutex
	waits   []time.Duration
	pending []chan time.Time
	asked   chan time.Duration
}

func newFakeAfter() *fakeAfter {
	return &fakeAfter{asked: make(chan time.Duration, 64)}
}

func (f *fakeAfter) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.pending = append(f.pending, ch)
	f.mu.Unlock()
	f.asked <- d
	return ch
}

// fire releases the oldest pending wait.
func (f *fakeAfter) fire(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		t.Fatal("no retry is waiting")
	}
	f.pending[0] <- time.Now()
	f.pending = f.pending[1:]
}

func (f *fakeAfter) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-f.asked:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the retry did not schedule a wait")
		return 0
	}
}

func waitLogState(t *testing.T, set *logFanoutSet, key, state string) logwatch.SourceStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := set.status(key)
		if st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("status of %s = %+v, want %s", key, st, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func appendDeadlockLine(t *testing.T, path string) {
	t.Helper()
	rec, err := json.Marshal(map[string]any{"timestamp": "2026-11-09 12:00:00.000 UTC",
		"pid": 7, "error_severity": "ERROR", "state_code": "40P01",
		"message": "deadlock detected", "dbname": "app"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(rec, '\n')); err != nil {
		t.Fatal(err)
	}
}

// waitDeadlockSignal waits for the deadlock line to reach the subscriber.
func waitDeadlockSignal(t *testing.T, sub interface{ Drain() []*rca.Signal }) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if sigs := sub.Drain(); len(sigs) > 0 {
			if sigs[0].ID != "log_deadlock_detected" {
				t.Fatalf("signal = %s, want log_deadlock_detected", sigs[0].ID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the early subscriber never received a signal after activation")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// State transition (CHECK-28): a log directory that appears later
// activates on the next retry, and a subscriber taken while the source was
// unavailable receives the signals once it is active.
func TestLogSourceRetryActivatesAfterDirectoryAppears(t *testing.T) {
	prepareMetaGlobals(t)
	dir := filepath.Join(t.TempDir(), "pg_log")
	cfg.LogWatch = config.LogWatchConfig{Enabled: logWatchSwitch(true), LogDirectory: dir,
		Format: "jsonlog", PollIntervalMs: 5, MaxLineLenBytes: 65536}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clock := newFakeAfter()
	set := newLogFanoutSet()
	set.after = clock.after
	fanout := set.forCluster(ctx, "db:5432", nil)
	if fanout == nil {
		t.Fatal("an unavailable source must still hand out a subscribable fanout")
	}
	sub := fanout.Subscribe("app")
	st := set.status("db:5432")
	if st.State != logwatch.SourceUnavailable || !strings.Contains(st.Reason, "log_directory") ||
		!strings.Contains(st.Reason, dir) {
		t.Fatalf("status = %+v, want unavailable naming log_directory %s", st, dir)
	}
	if d := clock.nextWait(t); d != logRetryMin {
		t.Fatalf("first retry wait = %v, want %v", d, logRetryMin)
	}
	if set.forCluster(ctx, "db:5432", nil) != fanout {
		t.Fatal("a second database replaced the pending fanout")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "postgresql.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clock.fire(t)
	st = waitLogState(t, set, "db:5432", logwatch.SourceActive)
	if st.File != path || st.Format != "jsonlog" || st.Reason != "" {
		t.Fatalf("active status = %+v, want file %s", st, path)
	}
	appendDeadlockLine(t, path)
	waitDeadlockSignal(t, sub)
	select {
	case d := <-clock.asked:
		t.Fatalf("an active source scheduled another retry (%v)", d)
	default:
	}
}

// Boundary: the backoff doubles from 30 s and holds at 10 min.
func TestNextLogRetry(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute,
		4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	d := time.Duration(0)
	for i, w := range want {
		d = nextLogRetry(d)
		if d != w {
			t.Fatalf("step %d: wait %v, want %v", i, d, w)
		}
	}
	if got := nextLogRetry(-time.Second); got != logRetryMin {
		t.Fatalf("negative wait gave %v", got)
	}
}

// State transition: a source that keeps failing backs off and the retry
// goroutine ends with its context.
func TestLogSourceRetryBacksOffAndStopsOnCancel(t *testing.T) {
	prepareMetaGlobals(t)
	cfg.LogWatch = config.LogWatchConfig{Enabled: logWatchSwitch(true), Format: "jsonlog",
		LogDirectory: filepath.Join(t.TempDir(), "never")}
	ctx, cancel := context.WithCancel(context.Background())
	clock := newFakeAfter()
	set := newLogFanoutSet()
	set.after = clock.after
	set.forCluster(ctx, "db:5432", nil)
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		if got := clock.nextWait(t); got != want {
			t.Fatalf("wait = %v, want %v", got, want)
		}
		clock.fire(t)
	}
	if got := clock.nextWait(t); got != 4*time.Minute {
		t.Fatalf("wait = %v, want 4m", got)
	}
	if st := set.status("db:5432"); st.State != logwatch.SourceUnavailable {
		t.Fatalf("still-broken source = %+v", st)
	}
	cancel()
	select {
	case <-set.retryDone("db:5432"):
	case <-time.After(5 * time.Second):
		t.Fatal("the retry goroutine outlived its context")
	}
}

// Nil/empty and defaults: with no config file the documented defaults hold
// and the source is disabled with the reason, not a zero-value status.
func TestLogSourceStatusDefaultsAndDisabled(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	lw := cfg.LogWatch
	if lw.Enabled != nil || lw.PollIntervalMs != 1000 || lw.DedupWindowS != 60 ||
		lw.MaxLineLenBytes != 65536 || lw.TempFileMinBytes != 10485760 ||
		lw.MaxLinesPerCycle != 10000 || lw.LogDirectory != "" || lw.Format != "" {
		t.Fatalf("logwatch defaults = %+v, want the documented ones", lw)
	}
	// rca-09b: the default is auto, so only an explicit false is disabled
	// by the switch itself.
	cfg.LogWatch.Enabled = logWatchSwitch(false)
	set := newLogFanoutSet()
	st := logSourceStatusFor(set, "db:5432", false)
	if st.State != logwatch.SourceDisabled || !strings.Contains(st.Reason, "logwatch.enabled") {
		t.Fatalf("switched-off status = %+v, want disabled naming logwatch.enabled", st)
	}
	cfg.LogWatch.Enabled = logWatchSwitch(true)
	cfg.RCA.Enabled, cfg.Migration.Enabled = false, false
	st = logSourceStatusFor(set, "db:5432", false)
	if st.State != logwatch.SourceDisabled || !strings.Contains(st.Reason, "rca.enabled") {
		t.Fatalf("no consumer status = %+v, want disabled naming rca.enabled", st)
	}
	cfg.RCA.Enabled = true
	st = logSourceStatusFor(set, "db:5432", false)
	if st.State != logwatch.SourceUnavailable || st.Reason == "" {
		t.Fatalf("enabled but never started = %+v, want unavailable with a reason", st)
	}
	if got := newLogFanoutSet().status("none:1"); got.State != "" {
		t.Fatalf("unknown cluster status = %+v, want unset", got)
	}
}

// Fleet: each cluster keeps its own status.
func TestLogSourceStatusIsPerCluster(t *testing.T) {
	prepareMetaGlobals(t)
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "postgresql.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set := newLogFanoutSet()
	set.after = newFakeAfter().after
	cfg.LogWatch = config.LogWatchConfig{Enabled: logWatchSwitch(true), LogDirectory: good,
		Format: "jsonlog", PollIntervalMs: 3600000}
	set.forCluster(ctx, "a:5432", nil)
	cfg.LogWatch.LogDirectory = filepath.Join(good, "missing")
	set.forCluster(ctx, "b:5432", nil)
	if st := set.status("a:5432"); st.State != logwatch.SourceActive {
		t.Fatalf("a = %+v, want active", st)
	}
	if st := set.status("b:5432"); st.State != logwatch.SourceUnavailable {
		t.Fatalf("b = %+v, want unavailable", st)
	}
	if drops := set.droppedLines(); len(drops) != 1 || drops[0].Cluster != "a:5432" {
		t.Fatalf("metrics must cover active watchers only: %+v", drops)
	}
}

// Concurrency: status reads while forCluster and the retry run.
func TestLogSourceStatusConcurrentReads(t *testing.T) {
	prepareMetaGlobals(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "postgresql.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.LogWatch = config.LogWatchConfig{Enabled: logWatchSwitch(true), LogDirectory: dir,
		Format: "jsonlog", PollIntervalMs: 1}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set := newLogFanoutSet()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				set.forCluster(ctx, "c:5432", nil)
				if st := set.status("c:5432"); st.State != logwatch.SourceActive {
					t.Errorf("status = %+v", st)
					return
				}
			}
		}()
	}
	wg.Wait()
}

type activeStub bool

func (a activeStub) Active() bool { return bool(a) }

// State transition: the SLO error proxy stays unknown while the shared log
// source is unavailable, so a retrying source never reads as zero errors.
func TestLogErrorCounterUnknownUntilSourceActive(t *testing.T) {
	sub := logwatch.NewFileWatcher(config.LogWatchConfig{}, nil).SubscribeEntries("slo", "app")
	t.Cleanup(sub.Stop)
	if n, ok := (logErrorCounter{sub: sub, source: activeStub(false)}).DrainErrors(); ok || n != 0 {
		t.Fatalf("inactive source = (%d, %v), want unknown", n, ok)
	}
	if n, ok := (logErrorCounter{sub: sub}).DrainErrors(); ok || n != 0 {
		t.Fatalf("no source = (%d, %v), want unknown", n, ok)
	}
	if n, ok := (logErrorCounter{sub: sub, source: activeStub(true)}).DrainErrors(); !ok || n != 0 {
		t.Fatalf("active empty source = (%d, %v), want a known zero", n, ok)
	}
}

// Error propagation: with no database pool left to read pg_settings, an
// auto-detected source is unavailable with the settings to set, never a
// nil-pool panic.
func TestLogSourceWithoutPoolNamesTheSettings(t *testing.T) {
	prepareMetaGlobals(t)
	cfg.LogWatch = config.LogWatchConfig{Enabled: logWatchSwitch(true), LogDirectory: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set := newLogFanoutSet()
	set.after = newFakeAfter().after
	set.forCluster(ctx, "db:5432", nil)
	st := set.status("db:5432")
	if st.State != logwatch.SourceUnavailable ||
		!strings.Contains(st.Reason, "logwatch.format") {
		t.Fatalf("status = %+v, want unavailable naming logwatch.format", st)
	}
}

// State transition: a removed database's pool is released, so the retry
// detects through a database that still runs, and nothing once none does.
func TestLogSourceReleasesRemovedDatabasePools(t *testing.T) {
	prepareMetaGlobals(t)
	cfg.LogWatch = config.LogWatchConfig{Enabled: logWatchSwitch(true), Format: "jsonlog",
		LogDirectory: filepath.Join(t.TempDir(), "missing")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set := newLogFanoutSet()
	set.after = newFakeAfter().after
	first, second := &pgxpool.Pool{}, &pgxpool.Pool{}
	set.forCluster(ctx, "db:5432", first)
	set.forCluster(ctx, "db:5432", second)
	latest := func() *pgxpool.Pool {
		set.mu.Lock()
		defer set.mu.Unlock()
		return set.clusters["db:5432"].latestPool()
	}
	if latest() != second {
		t.Fatal("the retry does not use the newest database's pool")
	}
	set.release("db:5432", second)
	if latest() != first {
		t.Fatal("a removed database's pool is still used for detection")
	}
	set.release("db:5432", second) // a second release is a no-op
	set.release("db:5432", first)
	if latest() != nil {
		t.Fatal("a pool outlived every database of the cluster")
	}
	set.release("none:1", first) // an unknown cluster is ignored
	set.release("db:5432", nil)
}

// A Supabase database whose RCA reads the provider's log API is active,
// not "unavailable" from the file watcher it does not use; the switches
// still win.
func TestLogSourceStatusProviderLogs(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	cfg.LogWatch.Enabled, cfg.RCA.Enabled = logWatchSwitch(true), true
	st := logSourceStatusFor(newLogFanoutSet(), "db.supabase.co:5432", true)
	if st.State != logwatch.SourceActive || st.Format != providerLogFormat || st.Reason != "" {
		t.Fatalf("provider status = %+v, want active %s", st, providerLogFormat)
	}
	cfg.LogWatch.Enabled = logWatchSwitch(false)
	st = logSourceStatusFor(newLogFanoutSet(), "db.supabase.co:5432", true)
	if st.State != logwatch.SourceDisabled {
		t.Fatalf("disabled logwatch with provider logs = %+v, want disabled", st)
	}
}
