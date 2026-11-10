//go:build integration

package logwatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// recordingSource feeds the engine from the real watcher and keeps every
// signal it handed over, so the test can check the classifier's metrics.
type recordingSource struct {
	fw   *FileWatcher
	seen []*rca.Signal
}

func (r *recordingSource) Start(ctx context.Context) error { return r.fw.Start(ctx) }
func (r *recordingSource) Stop()                           { r.fw.Stop() }
func (r *recordingSource) Drain() []*rca.Signal {
	sigs := r.fw.Drain()
	r.seen = append(r.seen, sigs...)
	return sigs
}

// crashServerEnv names a csvlog server whose log directory is visible to
// the test. scripts/capture_logwatch_corpus.py --serve starts one.
func crashServerEnv(t *testing.T) (url, container, dir string) {
	t.Helper()
	url = os.Getenv("SAGE_TEST_CSVLOG_URL")
	container = os.Getenv("SAGE_TEST_CSVLOG_CONTAINER")
	dir = os.Getenv("SAGE_TEST_CSVLOG_DIR")
	if url == "" || container == "" || dir == "" {
		t.Skip("SAGE_TEST_CSVLOG_URL, SAGE_TEST_CSVLOG_CONTAINER and " +
			"SAGE_TEST_CSVLOG_DIR not set: needs a csvlog server whose log " +
			"directory is mounted locally (scripts/capture_logwatch_corpus.py --serve)")
	}
	return url, container, dir
}

func victimPID(ctx context.Context, t *testing.T, url string) int {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect victim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("victim pid: %v", err)
	}
	return pid
}

func newCrashEngine() *rca.Engine {
	return rca.NewEngine(&config.RCAConfig{
		Enabled: true, DedupWindowMinutes: 30, EscalationCycles: 5,
		ResolutionCycles: 2, ConnectionSaturationPct: 80,
		ReplicationLagThresholdS: 30, WALSpikeMultiplier: 2.0,
	}, nopLog)
}

// CHECK-15: kill -9 of a backend opens a crash incident from the server's
// own csvlog, carrying the crashed PID and the signal number.
func TestIntegration_KilledBackendOpensCrashIncident(t *testing.T) {
	url, container, dir := crashServerEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A 1 s dedup window: on a reused server an earlier crash in the
	// replayed backlog would otherwise suppress this one for 60 s.
	src := &recordingSource{fw: NewFileWatcher(config.LogWatchConfig{
		LogDirectory: dir, Format: "csvlog", PollIntervalMs: 200,
		DedupWindowS: 1,
	}, nopLog)}
	if err := src.Start(ctx); err != nil {
		t.Fatalf("start watcher on %s: %v", dir, err)
	}
	defer src.Stop()
	// A reused server's log already holds earlier crashes; skip past them
	// so the incident below can only come from this kill.
	discardBacklog(src.fw)
	eng := newCrashEngine()
	eng.SetLogSource(src)

	pid := victimPID(ctx, t, url)
	out, err := exec.CommandContext(ctx, "docker", "exec", container,
		"sh", "-c", fmt.Sprintf("kill -9 %d", pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("kill -9 %d in %s: %v: %s", pid, container, err, out)
	}

	inc := waitForCrashIncident(ctx, t, eng)
	if inc.Severity != "critical" || inc.Source != "log_deterministic" {
		t.Errorf("incident severity/source = %s/%s, want critical/log_deterministic",
			inc.Severity, inc.Source)
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, fmt.Sprintf("(PID %d)", pid)) ||
		!strings.Contains(evidence, "signal 9") {
		t.Errorf("evidence %q does not name PID %d and signal 9", evidence, pid)
	}
	assertCrashMetrics(t, src.seen, pid)
}

func discardBacklog(fw *FileWatcher) {
	for i := 0; i < 5; i++ {
		fw.Drain()
		time.Sleep(300 * time.Millisecond)
	}
}

func waitForCrashIncident(ctx context.Context, t *testing.T, eng *rca.Engine) rca.Incident {
	t.Helper()
	cfg := &config.Config{RCA: config.RCAConfig{Enabled: true}}
	for ctx.Err() == nil {
		snap := &collector.Snapshot{CollectedAt: time.Now()}
		for _, inc := range eng.Analyze(snap, nil, cfg, nil) {
			if slices.Contains(inc.SignalIDs, "log_panic_server_crash") {
				return inc
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("no log_panic_server_crash incident within the deadline")
	return rca.Incident{}
}

func assertCrashMetrics(t *testing.T, seen []*rca.Signal, pid int) {
	t.Helper()
	for _, s := range seen {
		if s.ID != "log_panic_server_crash" {
			continue
		}
		if s.Metrics["signal"] != 9 || s.Metrics["crashed_pid"] != pid {
			t.Errorf("crash metrics signal=%v crashed_pid=%v, want 9/%d",
				s.Metrics["signal"], s.Metrics["crashed_pid"], pid)
		}
		return
	}
	t.Errorf("watcher never emitted log_panic_server_crash; saw %d signals", len(seen))
}
