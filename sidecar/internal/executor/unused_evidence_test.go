package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// An autonomous DROP of an "unused" index re-checks its evidence live just
// before acting: the analyzer resolves a finding whose window contains a
// statistics reset on its next cycle, but the executor may run before
// that. The drop is refused unless the index exists once, has zero scans,
// and the database's relation stats epoch is at least one unused window
// old.

func TestUnusedEvidence_Broken(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	window := 7 * 24 * time.Hour
	cases := []struct {
		name string
		ev   unusedEvidence
		want string // "" = evidence holds
	}{
		{"holds at the boundary", unusedEvidence{matches: 1, epoch: now.Add(-window)}, ""},
		{"reset inside the window",
			unusedEvidence{matches: 1, epoch: now.Add(-window + time.Second)}, "reset"},
		{"scanned", unusedEvidence{matches: 1, scans: 3, epoch: now.Add(-30 * 24 * time.Hour)},
			"scanned"},
		{"gone", unusedEvidence{epoch: now.Add(-30 * 24 * time.Hour)}, "not found"},
		{"ambiguous name", unusedEvidence{matches: 2, epoch: now.Add(-30 * 24 * time.Hour)},
			"ambiguous"},
		{"epoch unknown", unusedEvidence{matches: 1}, "unknown"},
	}
	for _, tc := range cases {
		got := tc.ev.broken(now, window)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: broken = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The unused window is analyzer.unused_index_window_days, 7 when unset.
func TestUnusedWindow(t *testing.T) {
	cfg := &config.Config{}
	if got := unusedWindow(cfg); got != 7*24*time.Hour {
		t.Fatalf("unset window = %s, want 7 days", got)
	}
	cfg.Analyzer.UnusedIndexWindowDays = 30
	if got := unusedWindow(cfg); got != 30*24*time.Hour {
		t.Fatalf("window = %s, want 30 days", got)
	}
}

func evidencePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "unused_evidence"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TABLE public.ev_t (id int PRIMARY KEY, a int);
		INSERT INTO public.ev_t SELECT g, g FROM generate_series(1, 500) g;
		CREATE INDEX ev_a ON public.ev_t (a); ANALYZE public.ev_t`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return pool
}

// The live read returns the index's scans and the database epoch, and the
// epoch moves when the statistics are reset.
func TestReadUnusedEvidence_RealStatistics(t *testing.T) {
	pool := evidencePool(t)
	ctx := context.Background()
	ev, err := readUnusedEvidence(ctx, pool, "public.ev_a")
	if err != nil || ev.matches != 1 || ev.scans != 0 || ev.epoch.IsZero() {
		t.Fatalf("evidence = %+v (%v), want one unscanned index with an epoch", ev, err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	moved := ev
	for !moved.epoch.After(ev.epoch) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if moved, err = readUnusedEvidence(ctx, pool, "public.ev_a"); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !moved.epoch.After(ev.epoch) {
		t.Fatalf("epoch did not move on pg_stat_reset: %s", moved.epoch)
	}
	if gone, err := readUnusedEvidence(ctx, pool, "public.nope"); err != nil || gone.matches != 0 {
		t.Fatalf("missing index = %+v (%v), want no match", gone, err)
	}
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(_ string, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *logRecorder) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// processFinding refuses an unused-index drop whose evidence is broken by
// a fresh reset, before any policy, queue or apply step. A duplicate-index
// drop does not rest on usage counters and is not gated.
func TestProcessFinding_RefusesUnusedDropAfterReset(t *testing.T) {
	pool := evidencePool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	rec := &logRecorder{}
	e := New(pool, &config.Config{}, time.Now().Add(-365*24*time.Hour), rec.log)
	unused := analyzer.Finding{Category: "unused_index", ObjectIdentifier: "public.ev_a",
		Title: "Unused index public.ev_a", RecommendedSQL: "DROP INDEX CONCURRENTLY public.ev_a",
		ActionRisk: "safe"}
	e.processFinding(ctx, unused, false, nil)
	if !rec.has("unused-index evidence") || !rec.has("reset") {
		t.Fatalf("logs = %v, want the drop refused for a reset inside the window", rec.lines)
	}
	dup := unused
	dup.Category = "duplicate_index"
	rec2 := &logRecorder{}
	e2 := New(pool, &config.Config{}, time.Now().Add(-365*24*time.Hour), rec2.log)
	e2.processFinding(ctx, dup, false, nil)
	if rec2.has("unused-index evidence") {
		t.Fatalf("duplicate_index drop gated on usage evidence: %v", rec2.lines)
	}
}
