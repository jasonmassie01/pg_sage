package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-09b: with logwatch.enabled unset (auto) log RCA starts when a
// readable jsonlog or csvlog source is detected, and stays quietly
// disabled, still probing, while none is.

// logWatchSwitch is an explicit logwatch.enabled value.
func logWatchSwitch(on bool) *bool { return &on }

// captureLogs sends the sidecar's log lines to a buffer for the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := setLogOutput(buf)
	t.Cleanup(func() { setLogOutput(old) })
	return buf
}

// Phrases of the log-source failure lines: explicit true WARNs, auto
// notes once at INFO.
const (
	warnPhrase = "log source unavailable"
	infoPhrase = "waits for a readable log source"
)

// countLogs counts logwatch lines of level that contain phrase; lines a
// previous test's goroutines write (a watcher stopping) do not match.
func countLogs(buf *syncBuffer, level, phrase string) int {
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "["+level+"] [logwatch]") && strings.Contains(line, phrase) {
			n++
		}
	}
	return n
}

// switchableResolver is a fake pg_settings detection whose answer the
// test changes between retries.
type switchableResolver struct {
	mu       sync.Mutex
	settings config.LogWatchConfig
	err      error
	calls    int
}

func (r *switchableResolver) resolve(
	context.Context, *pgxpool.Pool,
) (config.LogWatchConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.settings, r.err
}

func (r *switchableResolver) set(settings config.LogWatchConfig, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settings, r.err = settings, err
}

// stderrOnly is the detector's refusal of a server that logs to stderr.
var stderrOnly = fmt.Errorf("%w: log_destination is %q; pg_sage reads only jsonlog "+
	"(PostgreSQL 15+) or csvlog files", logwatch.ErrLogSourceUnsupported, "stderr")

func appendCSVDeadlockLine(t *testing.T, path string) {
	t.Helper()
	cols := make([]string, 26)
	cols[0], cols[2], cols[3] = "2026-11-13 12:00:00.000 UTC", "app", "7"
	cols[11], cols[12], cols[13] = "ERROR", "40P01", "deadlock detected"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(cols, ",") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func autoSet(t *testing.T, r *switchableResolver) (*logFanoutSet, *fakeAfter) {
	t.Helper()
	clock := newFakeAfter()
	set := newLogFanoutSet()
	set.after, set.resolve = clock.after, r.resolve
	return set, clock
}

// Defaults: with no config file a log source is wired (auto) for RCA;
// explicit false, or no log consumer, wires none.
func TestLogWatchWantedByDefault(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	if cfg.LogWatch.Mode() != config.LogWatchAuto || !logWatchWanted() {
		t.Fatalf("default mode %q wanted=%v, want auto and wired",
			cfg.LogWatch.Mode(), logWatchWanted())
	}
	off := false
	cfg.LogWatch.Enabled = &off
	if logWatchWanted() {
		t.Fatal("logwatch.enabled: false still wires a log source")
	}
	cfg.LogWatch.Enabled = nil
	cfg.RCA.Enabled, cfg.Migration.Enabled = false, false
	if logWatchWanted() {
		t.Fatal("auto wired a log source with no log consumer")
	}
}

// State transition and happy path: auto on a stderr-only server is
// disabled with the detector's reason and one INFO line; once the server
// writes csvlog the next retry activates it and a deadlock reaches RCA.
func TestLogSourceAutoActivatesWhenCSVLogAppears(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	cfg.LogWatch.PollIntervalMs = 5
	logs := captureLogs(t)
	r := &switchableResolver{err: stderrOnly}
	dir := t.TempDir() // made first: removed only after cancel stops the watcher
	set, clock := autoSet(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := set.forCluster(ctx, "db:5432", nil).Subscribe("app")
	if d := clock.nextWait(t); d != logRetryMin {
		t.Fatalf("auto did not keep probing: first wait %v", d)
	}
	st := set.status("db:5432")
	if st.State != logwatch.SourceDisabled || !strings.HasPrefix(st.Reason, "auto: ") ||
		!strings.Contains(st.Reason, "log_destination") || len(st.Hints) == 0 ||
		st.NextRetryAt == nil {
		t.Fatalf("auto stderr status = %+v, want disabled auto naming log_destination", st)
	}
	if w, i := countLogs(logs, "WARN", warnPhrase),
		countLogs(logs, "INFO", infoPhrase); w != 0 || i != 1 {
		t.Fatalf("auto stderr logged %d WARN and %d INFO, want 0 and 1:\n%s", w, i, logs)
	}
	path := filepath.Join(dir, "postgresql.csv")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.set(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog"}, nil)
	clock.fire(t)
	st = waitLogState(t, set, "db:5432", logwatch.SourceActive)
	if st.Format != "csvlog" || st.File != path || st.Reason != "" {
		t.Fatalf("activated status = %+v, want csvlog %s", st, path)
	}
	appendCSVDeadlockLine(t, path)
	waitDeadlockSignal(t, sub)
}

// Error propagation: a detection query error in auto mode is the
// disabled reason, logged once per streak, never as a WARN storm.
func TestLogSourceAutoDetectErrorLogsOncePerStreak(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	logs := captureLogs(t)
	r := &switchableResolver{err: errors.New("logwatch detect: query pg_settings: " +
		"connection refused")}
	set, clock := autoSet(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set.forCluster(ctx, "db:5432", nil)
	for range 3 {
		clock.nextWait(t)
		clock.fire(t)
	}
	clock.nextWait(t)
	st := set.status("db:5432")
	if st.State != logwatch.SourceDisabled || !strings.Contains(st.Reason, "connection refused") {
		t.Fatalf("status = %+v, want disabled with the query error", st)
	}
	if r.calls < 4 {
		t.Fatalf("detection ran %d times, want every retry to probe", r.calls)
	}
	if w, i := countLogs(logs, "WARN", warnPhrase),
		countLogs(logs, "INFO", infoPhrase); w != 0 || i != 1 {
		t.Fatalf("logged %d WARN and %d INFO over 4 attempts, want 0 and 1:\n%s", w, i, logs)
	}
}

// State transition: explicit true keeps today's unavailable state and
// WARN for an unreadable source.
func TestLogSourceExplicitTrueKeepsWarn(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	on := true
	cfg.LogWatch.Enabled = &on
	logs := captureLogs(t)
	set, clock := autoSet(t, &switchableResolver{err: stderrOnly})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set.forCluster(ctx, "db:5432", nil)
	clock.nextWait(t)
	st := set.status("db:5432")
	if st.State != logwatch.SourceUnavailable || strings.HasPrefix(st.Reason, "auto:") ||
		!strings.Contains(st.Reason, "log_destination") {
		t.Fatalf("explicit true status = %+v, want unavailable naming log_destination", st)
	}
	if countLogs(logs, "WARN", warnPhrase) != 1 {
		t.Fatalf("explicit true must WARN once on the first failure:\n%s", logs)
	}
}

// State transition: explicit false never activates, even with a readable
// source and a provider log sink.
func TestLogSourceExplicitFalseNeverActivates(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	off := false
	cfg.LogWatch.Enabled = &off
	for _, provider := range []bool{false, true} {
		st := logSourceStatusFor(newLogFanoutSet(), "db:5432", provider)
		if st.State != logwatch.SourceDisabled || st.Reason != "logwatch.enabled is false" {
			t.Fatalf("provider=%v: status = %+v, want disabled by logwatch.enabled",
				provider, st)
		}
	}
}

// Managed providers: auto counts a configured provider log sink as
// detected.
func TestLogSourceAutoProviderSinkIsActive(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	st := logSourceStatusFor(newLogFanoutSet(), "db.supabase.co:5432", true)
	if st.State != logwatch.SourceActive || st.Format != providerLogFormat {
		t.Fatalf("auto provider status = %+v, want active %s", st, providerLogFormat)
	}
}

// Concurrency: status reads while the retry goroutine activates the
// source (meaningful under -race in CI).
func TestLogSourceAutoStatusReadsDuringActivation(t *testing.T) {
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	cfg.LogWatch.PollIntervalMs = 1
	captureLogs(t)
	r := &switchableResolver{err: stderrOnly}
	dir := t.TempDir() // made first: removed only after cancel stops the watcher
	set, clock := autoSet(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	set.forCluster(ctx, "c:5432", nil)
	clock.nextWait(t)
	if err := os.WriteFile(filepath.Join(dir, "postgresql.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.set(config.LogWatchConfig{LogDirectory: dir, Format: "jsonlog"}, nil)
	var wg sync.WaitGroup
	seen := make(chan string, 8*200)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				seen <- set.status("c:5432").State
			}
		}()
	}
	clock.fire(t)
	wg.Wait()
	close(seen)
	for state := range seen {
		if state != logwatch.SourceDisabled && state != logwatch.SourceActive {
			t.Fatalf("a concurrent read saw state %q", state)
		}
	}
	waitLogState(t, set, "c:5432", logwatch.SourceActive)
}
