package analyzer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// static.md F11: the analyzer's database checks ran with no server-side
// statement timeout. Each now runs through catalogread under
// safety.query_timeout_ms; a slow read is cut off and the cycle degrades
// (the check fails closed) instead of hanging.

type timeoutLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *timeoutLog) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *timeoutLog) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// slowReads makes every bounded read sleep inside its transaction first.
func slowReads(calls *atomic.Int32) context.Context {
	return catalogread.WithBeforeStatement(context.Background(),
		func(ctx context.Context, tx pgx.Tx) error {
			calls.Add(1)
			_, err := tx.Exec(ctx, "SELECT pg_sleep(10)")
			return err
		})
}

func boundedReadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestAnalyzerChecks_SlowReadsCutOffAndFailClosed(t *testing.T) {
	pool := boundedReadPool(t)
	cfg := &config.Config{Safety: config.SafetyConfig{QueryTimeoutMs: 200}}
	cfg.Analyzer.WorkMemPromotionThreshold = 1
	logs := &timeoutLog{}
	a := New(pool, cfg, nil, nil, nil, nil, nil, logs.log)
	a.eval = newCycleEval()
	var calls atomic.Int32
	ctx := slowReads(&calls)
	start := time.Now()

	a.loadRecentlyCreatedIndexes(ctx)
	a.loadStatsEpoch(ctx)
	a.loadIndexBuilds(ctx)
	if f := a.checkXIDWraparound(ctx); f != nil {
		t.Errorf("xid check returned %v on a timed-out read", f)
	}
	if f := a.checkConnectionLeaks(ctx); len(f) != 0 {
		t.Errorf("leak check returned %v on a timed-out read", f)
	}
	if f := a.checkExtensionDrift(ctx); len(f) != 0 {
		t.Errorf("extension drift returned %v", f)
	}
	if f := a.checkSortWithoutIndex(ctx); len(f) != 0 {
		t.Errorf("sort check returned %v", f)
	}
	if f := a.checkWorkMemPromotion(ctx); len(f) != 0 {
		t.Errorf("work_mem promotion returned %v", f)
	}
	if tables := a.openIndexRecommendationTables(ctx); len(tables) != 0 {
		t.Errorf("open index tables = %v", tables)
	}
	if _, _, err := a.loadCloneSessions(ctx); err == nil {
		t.Error("clone sessions read returned no error")
	}
	if _, err := a.measureSageFootprint(ctx); err == nil {
		t.Error("sage footprint read returned no error")
	}
	in := []Finding{{Category: "unused_index", ObjectIdentifier: "public.x_idx"}}
	if out := a.applyAppManaged(ctx, in); len(out) != 1 {
		t.Errorf("app-managed marking dropped findings: %v", out)
	}

	assertBoundedChecks(t, a, logs, calls.Load(), time.Since(start))
}

func assertBoundedChecks(t *testing.T, a *Analyzer, logs *timeoutLog, calls int32,
	elapsed time.Duration) {
	t.Helper()
	if calls < 12 {
		t.Fatalf("%d reads went through the bounded helper, want all 12 checks", calls)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("12 timed-out checks took %s, want each cut off at 200ms", elapsed)
	}
	if n := logs.count("statement timeout"); n < 10 {
		t.Fatalf("%d logged statement timeouts, want one per failed check: %v", n, logs.lines)
	}
	for _, c := range []string{"xid_wraparound", "connection_leak", "unused_index"} {
		if !a.eval.failed[c] {
			t.Errorf("category %s not failed closed after a timed-out read", c)
		}
	}
	if !a.extras.IndexBuildProbeFailed {
		t.Error("index build probe not marked failed")
	}
	if time.Since(a.extras.StatsEpoch) > time.Minute {
		t.Errorf("stats epoch %v, want now (fail closed)", a.extras.StatsEpoch)
	}
}

func TestAnalyzerCatalogReader_UsesSafetyTimeouts(t *testing.T) {
	a := New(nil, &config.Config{Safety: config.SafetyConfig{QueryTimeoutMs: 321,
		LockTimeoutMs: 4000}}, nil, nil, nil, nil, nil, func(string, string, ...any) {})
	if got := a.catalog().Timeouts; got.Statement != 321*time.Millisecond ||
		got.Lock != 4*time.Second {
		t.Fatalf("analyzer catalog timeouts = %+v", got)
	}
	a.cfg = nil
	if got := a.catalog().Timeouts; got != catalogread.Default() {
		t.Fatalf("no config: timeouts = %+v, want the defaults", got)
	}
}
