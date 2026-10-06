package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/benchingest"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	srebench "github.com/pg-sage/sidecar/sre-bench"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// bench_results_path pointed at the CI artifact pgincidentbench-pg17
// (three shard reports and the top-level replay corpus report) ingests
// the three bench reports with no error, so no hourly WARN, and says once
// per file, at INFO, that a replay report was passed over.

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.lines...)
}

func TestReplayNoticesLogEachSkippedFileOnce(t *testing.T) {
	var notices replayNotices
	var log logLines
	a := filepath.Join("ci", "pgincidentbench-replay.json")
	b := filepath.Join("other", "pgincidentbench-replay.json")
	notices.note(nil, log.logf)
	notices.note([]string{}, log.logf)
	if got := log.all(); len(got) != 0 {
		t.Fatalf("nothing skipped logged %v", got)
	}
	notices.note([]string{a}, log.logf)
	notices.note([]string{a}, log.logf) // the next hourly tick
	got := log.all()
	if len(got) != 1 || !strings.Contains(got[0], a) ||
		!strings.Contains(got[0], "replay-corpus report") {
		t.Fatalf("after two ticks: %v, want one notice naming %s", got, a)
	}
	notices.note([]string{a, b}, log.logf)
	got = log.all()
	if len(got) != 2 || !strings.Contains(got[1], b) {
		t.Fatalf("a second replay report: %v, want one more notice naming %s", got, b)
	}
}

func TestReplayNoticesConcurrentTicksLogOnce(t *testing.T) {
	var notices replayNotices
	var log logLines
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			notices.note([]string{"r.json"}, log.logf)
		}()
	}
	wg.Wait()
	if got := log.all(); len(got) != 1 {
		t.Fatalf("16 concurrent ledgers logged %d notices, want 1: %v", len(got), got)
	}
}

// The real ledger: three reports stored, the replay report skipped, no
// error for the caller to WARN about.
func TestOperatorPathCIArtifactIngestsThreeReports(t *testing.T) {
	asRelease(t)
	withVerifier(t, nil, nil)
	pool := autonomyPool(t)
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool,
		"replayskip", config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	build := earned.Build{Version: "1.8.5", Commit: releaseCommit}
	at := time.Now().UTC().Add(-time.Minute)
	for shard, family := range map[string]string{"core": "lock_blocking",
		"reactive": "checkpoint_storm", "runway": "wal_retention"} {
		writeShippedReport(t, root, shard, at, build, family, false)
	}
	replayPath, _, err := srebench.WriteReplayReport(root, srebench.ReplayReport{
		Schema: replay.Schema, GeneratedAt: at, Cases: 3,
		LLM: srebench.LLMConfig{Mode: "off"}, Arms: []string{"causal-graph"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ingestBenchSources(context.Background(), svc, []benchingest.Source{
		{Path: root, Actor: "bench_results_path"}})
	if err != nil {
		t.Fatalf("ingest of the CI artifact: %v", err)
	}
	if res.Files != 3 || res.Added != 3 || len(res.Skipped) != 1 ||
		res.Skipped[0] != replayPath {
		t.Fatalf("result = %+v, want 3 reports and the replay report skipped", res)
	}
}
