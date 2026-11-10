package logwatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

const statusOOMLine = `{"timestamp":"2026-11-08 12:00:00.000 UTC","pid":1,` +
	`"error_severity":"ERROR","state_code":"53200","message":"out of memory"}`

// startStatusWatcher starts a jsonlog watcher on an empty file in a fresh
// directory; the hour-long poll leaves every read to Drain.
func startStatusWatcher(t *testing.T) (*FileWatcher, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "jsonlog",
		PollIntervalMs: 3600000, MaxLineLenBytes: 65536}, nil)
	if err := fw.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(fw.Stop)
	return fw, path
}

func appendStatusLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// Happy path: an active status names the file and the time of the last line.
func TestFileWatcherStatusActiveAfterOneLine(t *testing.T) {
	fw, path := startStatusWatcher(t)
	st := fw.Status()
	if st.State != SourceActive || st.File != path || st.Format != "jsonlog" {
		t.Fatalf("started status = %+v, want active on %s", st, path)
	}
	if st.LastLineAt != nil {
		t.Fatalf("no line read yet, LastLineAt = %v", st.LastLineAt)
	}
	before := time.Now()
	appendStatusLine(t, path, statusOOMLine)
	if got := fw.Drain(); len(got) != 1 {
		t.Fatalf("drain = %d signals, want 1", len(got))
	}
	st = fw.Status()
	if st.LastLineAt == nil || st.LastLineAt.Before(before) ||
		st.LastLineAt.After(time.Now()) {
		t.Fatalf("LastLineAt = %v, want a time after %v", st.LastLineAt, before)
	}
	if st.ParseErrors != 0 || st.DroppedLines != 0 || st.Reason != "" {
		t.Fatalf("clean drain status = %+v", st)
	}
}

// Error propagation: unparseable records are counted, not hidden.
func TestFileWatcherStatusCountsParseErrors(t *testing.T) {
	fw, path := startStatusWatcher(t)
	appendStatusLine(t, path, "{not json")
	appendStatusLine(t, path, "{also not json")
	fw.Drain()
	if st := fw.Status(); st.ParseErrors != 2 || st.LastLineAt == nil {
		t.Fatalf("status = %+v, want 2 parse errors and a LastLineAt", st)
	}
	appendStatusLine(t, path, statusOOMLine)
	fw.Drain()
	if st := fw.Status(); st.ParseErrors != 2 {
		t.Fatalf("parse errors must be cumulative, got %d", st.ParseErrors)
	}
}

// Nil/empty: a watcher that never started is not reported active.
func TestFileWatcherStatusBeforeStart(t *testing.T) {
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: t.TempDir()}, nil)
	st := fw.Status()
	if st.State != SourceUnavailable || !strings.Contains(st.Reason, "not started") {
		t.Fatalf("unstarted status = %+v, want unavailable/not started", st)
	}
}

// Nil/empty: an unset status reads as disabled with its reason, never as a
// zero-value struct.
func TestSourceStatusOrDisabled(t *testing.T) {
	got := SourceStatus{}.OrDisabled("logwatch.enabled is false")
	if got.State != SourceDisabled || got.Reason != "logwatch.enabled is false" {
		t.Fatalf("zero status = %+v, want disabled", got)
	}
	active := SourceStatus{State: SourceActive, File: "/x"}
	if got := active.OrDisabled("unused"); got.State != SourceActive || got.Reason != "" {
		t.Fatalf("set status replaced: %+v", got)
	}
	if d := DisabledStatus("r"); d.State != SourceDisabled || d.Reason != "r" {
		t.Fatalf("DisabledStatus = %+v", d)
	}
	u := UnavailableStatus("log_directory /x does not exist")
	if u.State != SourceUnavailable || u.Reason != "log_directory /x does not exist" {
		t.Fatalf("UnavailableStatus = %+v", u)
	}
	if u := UnavailableStatus(""); u.Reason == "" {
		t.Fatal("an unavailable status must carry a reason")
	}
}

// Error propagation: log_destination=stderr names the setting to change.
func TestResolveFormatStderrNamesTheSetting(t *testing.T) {
	_, err := resolveFormat("stderr")
	if err == nil {
		t.Fatal("stderr accepted")
	}
	for _, want := range []string{"log_destination", `"stderr"`, "jsonlog",
		"csvlog", "logging_collector"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	if !errors.Is(err, ErrLogSourceUnsupported) {
		t.Fatalf("error %v is not ErrLogSourceUnsupported", err)
	}
}

// Error propagation: csvlog/jsonlog without the collector writes no file.
func TestCheckCollector(t *testing.T) {
	if err := checkCollector("on"); err != nil {
		t.Fatalf("collector on rejected: %v", err)
	}
	for _, v := range []string{"off", ""} {
		err := checkCollector(v)
		if err == nil || !strings.Contains(err.Error(), "logging_collector") ||
			!errors.Is(err, ErrLogSourceUnsupported) {
			t.Fatalf("checkCollector(%q) = %v, want an error naming logging_collector", v, err)
		}
	}
}

// Boundary: log_temp_files -1 hides temp-file signals, 0 logs all of them.
func TestSignalSettingHints(t *testing.T) {
	hints := SignalSettingHints(map[string]string{"log_temp_files": "-1",
		"log_min_duration_statement": "-1"}, true)
	joined := strings.Join(hints, "\n")
	if len(hints) != 2 || !strings.Contains(joined, "log_temp_files") ||
		!strings.Contains(joined, "log_min_duration_statement") {
		t.Fatalf("hints = %q, want log_temp_files and log_min_duration_statement", hints)
	}
	if got := SignalSettingHints(map[string]string{"log_temp_files": "-1",
		"log_min_duration_statement": "-1"}, false); len(got) != 1 {
		t.Fatalf("slow-query hint without slow_query_enabled: %q", got)
	}
	if got := SignalSettingHints(map[string]string{"log_temp_files": "0",
		"log_min_duration_statement": "1000"}, true); len(got) != 0 {
		t.Fatalf("enabled settings gave hints %q", got)
	}
	if got := SignalSettingHints(nil, true); len(got) != 0 {
		t.Fatalf("unreadable settings gave hints %q", got)
	}
}

// Concurrency: status reads while the drain runs (go test -race where cgo
// is available; here it checks reads never block or panic mid-drain).
func TestFileWatcherStatusDuringDrain(t *testing.T) {
	fw, path := startStatusWatcher(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			appendStatusLine(t, path, statusOOMLine)
			fw.Drain()
		}
		close(stop)
	}()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if st := fw.Status(); st.State != SourceActive {
						t.Errorf("status mid-drain = %+v", st)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if st := fw.Status(); st.LastLineAt == nil || st.ParseErrors != 0 {
		t.Fatalf("final status = %+v", st)
	}
}

// State transition: StartWithConfig applies the detected source before the
// first start, so a watcher created before detection can still start.
func TestFileWatcherStartWithConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.csv")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fw := NewFileWatcher(config.LogWatchConfig{PollIntervalMs: 3600000}, nil)
	if err := fw.StartWithConfig(t.Context(), config.LogWatchConfig{LogDirectory: dir,
		Format: "csvlog", PollIntervalMs: 3600000}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(fw.Stop)
	if st := fw.Status(); st.State != SourceActive || st.Format != "csvlog" || st.File != path {
		t.Fatalf("status = %+v, want active csvlog on %s", st, path)
	}
}

// State transition: Active follows Start and Stop, for the watcher and
// for a fanout built on it before it started.
func TestFileWatcherActiveFollowsLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "postgresql.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "jsonlog",
		PollIntervalMs: 3600000}, nil)
	fanout := NewLogFanout(fw)
	if fw.Active() || fanout.Active() {
		t.Fatal("unstarted watcher reported active")
	}
	if err := fw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !fw.Active() || !fanout.Active() {
		t.Fatal("started watcher not active")
	}
	fw.Stop()
	if fw.Active() || fw.Status().State != SourceUnavailable {
		t.Fatalf("stopped watcher still active: %+v", fw.Status())
	}
}

// Error propagation: a failed start leaves the watcher inactive.
func TestFileWatcherFailedStartStaysInactive(t *testing.T) {
	fw := NewFileWatcher(config.LogWatchConfig{
		LogDirectory: filepath.Join(t.TempDir(), "missing"), Format: "jsonlog"}, nil)
	if err := fw.Start(t.Context()); err == nil {
		t.Fatal("missing directory started")
	}
	if fw.Active() || fw.Status().State != SourceUnavailable {
		t.Fatalf("failed start reported %+v", fw.Status())
	}
}

// Error propagation: log_directory is hidden from a role without
// pg_read_all_settings; the error says how to fix that.
func TestResolveLogDirEmptyNamesTheGrant(t *testing.T) {
	_, err := resolveLogDir("", "")
	if err == nil || !strings.Contains(err.Error(), "pg_read_all_settings") ||
		!strings.Contains(err.Error(), "logwatch.log_directory") {
		t.Fatalf("empty log_directory error = %v, want the grant and the setting", err)
	}
}
