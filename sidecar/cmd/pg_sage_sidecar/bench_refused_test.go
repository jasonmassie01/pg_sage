package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/benchingest"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// earned-10: after an upgrade every report under bench_results_path that
// is stamped for the old build is refused (ErrBuildMismatch). The hourly
// ingest of every database logs each refused file once per process, at
// INFO, naming the build it scores; a rewritten file (a new hash) logs
// again. Other ingest errors still WARN on every tick.

var staleBuild = earned.Build{Version: "1.8.4",
	Commit: "dddddddddddddddddddddddddddddddddddddddd"}

func TestRefusalNoticesLogEachFileVersionOnce(t *testing.T) {
	var notices benchNotices
	var log logLines
	a := benchingest.Refused{Path: "a.json", SHA256: strings.Repeat("1", 64),
		Reason: "it scores pg_sage 1.8.4, this is pg_sage 1.8.5"}
	notices.noteRefused(nil, log.logf)
	notices.noteRefused([]benchingest.Refused{}, log.logf)
	if got := log.all(); len(got) != 0 {
		t.Fatalf("nothing refused logged %v", got)
	}
	notices.noteRefused([]benchingest.Refused{a}, log.logf)
	notices.noteRefused([]benchingest.Refused{a}, log.logf) // the next hourly tick
	got := log.all()
	if len(got) != 1 || !strings.Contains(got[0], "a.json") ||
		!strings.Contains(got[0], "pg_sage 1.8.4") || !strings.Contains(got[0], "111111111111") {
		t.Fatalf("after two ticks: %v, want one notice naming a.json, its hash and build", got)
	}
	rewritten := a
	rewritten.SHA256 = strings.Repeat("2", 64)
	notices.noteRefused([]benchingest.Refused{rewritten, a}, log.logf)
	if got = log.all(); len(got) != 2 || !strings.Contains(got[1], "222222222222") {
		t.Fatalf("a rewritten file: %v, want one more notice for the new hash", got)
	}
	// A refusal and a replay skip of the same path are different notices.
	notices.note([]string{"a.json"}, log.logf)
	if got = log.all(); len(got) != 3 || !strings.Contains(got[2], "replay-corpus") {
		t.Fatalf("a replay skip of a refused path: %v, want its own notice", got)
	}
}

func TestRefusalNoticesConcurrentTicksLogOnce(t *testing.T) {
	var notices benchNotices
	var log logLines
	var wg sync.WaitGroup
	r := benchingest.Refused{Path: "r.json", SHA256: strings.Repeat("3", 64), Reason: "x"}
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			notices.noteRefused([]benchingest.Refused{r}, log.logf)
		}()
	}
	wg.Wait()
	if got := log.all(); len(got) != 1 {
		t.Fatalf("16 concurrent ledgers logged %d notices, want 1: %v", len(got), got)
	}
}

// matching are the lines of out containing every one of parts.
func matching(out string, parts ...string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		ok := line != ""
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			lines = append(lines, line)
		}
	}
	return lines
}

// The real hourly ingest of two databases over one stale report: one
// INFO line over two ticks each, no WARN; rewriting the file logs again;
// a malformed file still WARNs on every tick.
func TestBenchIngesterLogsAStaleReportOncePerProcess(t *testing.T) {
	asRelease(t)
	withShippedDirs(t)
	withVerifier(t, nil, nil)
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	s := config.DefaultConfig().SRE.Autonomy
	s.BenchResultsPath = t.TempDir()
	stale := writeShippedReport(t, s.BenchResultsPath, "core", time.Now().UTC(), staleBuild,
		"wal_retention", false)
	var ticks []func(context.Context)
	for _, db := range []string{"stale_a", "stale_b"} {
		svc, err := ledgers.ledgerFor(context.Background(), pool, db, s)
		if err != nil {
			t.Fatal(err)
		}
		ticks = append(ticks, benchIngester(svc, s))
	}
	tick := func() {
		for _, fn := range ticks {
			fn(context.Background())
		}
	}
	out := captureStderr(t, func() { tick(); tick() })
	refused := matching(out, "[INFO]", stale, "pg_sage 1.8.4")
	if len(refused) != 1 || len(matching(out, "[WARN]", "ingest bench reports")) != 0 {
		t.Fatalf("two ticks of two databases logged:\n%s\nwant one INFO refusal", out)
	}
	writeShippedReport(t, s.BenchResultsPath, "core", time.Now().UTC().Add(-time.Minute),
		staleBuild, "wal_retention", false)
	broken := filepath.Join(s.BenchResultsPath, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"schema": `), 0o600); err != nil {
		t.Fatal(err)
	}
	out = captureStderr(t, func() { tick(); tick() })
	if got := matching(out, "[INFO]", stale, "pg_sage 1.8.4"); len(got) != 1 {
		t.Fatalf("after a rewrite:\n%s\nwant one INFO refusal of the new version", out)
	}
	if got := matching(out, "[WARN]", "broken.json"); len(got) != 4 {
		t.Fatalf("a malformed file:\n%s\nwant a WARN on every tick of every database", out)
	}
	if got := matching(out, "[WARN]", stale); len(got) != 0 {
		t.Fatalf("the stale report was warned about:\n%s", out)
	}
}
