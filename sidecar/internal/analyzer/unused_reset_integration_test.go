package analyzer

import (
	"context"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// resetFixture is a database with one table and two secondary indexes,
// a real collector sampling it every second and an analyzer on a fake
// clock.
type resetFixture struct {
	pool    *pgxpool.Pool
	version int
	coll    *collector.Collector
	a       *Analyzer
	now     time.Time
	stop    func()
}

func newResetFixture(t *testing.T) *resetFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "unused_reset"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE public.reset_t (id int PRIMARY KEY, a int, b int);
		INSERT INTO public.reset_t SELECT g, g, g FROM generate_series(1, 2000) g;
		CREATE INDEX ix_seen ON public.reset_t (a);
		CREATE INDEX ix_unseen ON public.reset_t (b);
		ANALYZE public.reset_t`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	f := &resetFixture{pool: pool}
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).
		Scan(&f.version); err != nil {
		t.Fatalf("version: %v", err)
	}
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 1
	cfg.Safety.CPUCeilingPct = 100
	f.coll = collector.New(pool, cfg, f.version, noopLog)
	f.a = New(pool, cfg, f.coll, nil, nil, nil, nil, noopLog)
	f.a.extras.Now = func() time.Time { return f.now }
	f.startCollector(t)
	t.Cleanup(func() { f.stop() })
	return f
}

func (f *resetFixture) startCollector(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.coll.Run(ctx) }()
	f.stop = func() { cancel(); <-done }
}

// scanIndex runs index scans on column col and waits until the statistics
// show them (a forced flush on PG15+, the stats collector's delay before).
func (f *resetFixture) scanIndex(t *testing.T, index, col string) {
	t.Helper()
	ctx := context.Background()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	const planner = `SET enable_seqscan = off; SET enable_bitmapscan = off`
	if _, err := conn.Exec(ctx, planner); err != nil {
		t.Fatalf("planner settings: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := conn.Exec(ctx, "SELECT * FROM public.reset_t WHERE "+col+" = "+
			strconv.Itoa(i)); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	// All five scans must be counted before the test goes on: on PG14 a
	// report can carry the first scan while the rest stay pending in this
	// session, to arrive after the pg_stat_reset() that follows and show
	// scans the reset should have erased.
	if err := testdb.FlushStats(ctx, conn); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	f.waitFor(t, "scans of "+index+" visible", func() bool {
		return f.liveScans(t, index) >= 5
	})
}

func (f *resetFixture) liveScans(t *testing.T, index string) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), `SELECT idx_scan FROM pg_stat_user_indexes
		WHERE indexrelname = $1`, index).Scan(&n); err != nil {
		t.Fatalf("idx_scan: %v", err)
	}
	return n
}

func (f *resetFixture) waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitSnapshot waits for a snapshot newer than the last analyzed one that
// satisfies ok.
func (f *resetFixture) waitSnapshot(t *testing.T, what string, ok func(*collector.Snapshot) bool) {
	t.Helper()
	after := f.a.lastAnalyzedAt
	f.waitFor(t, what, func() bool {
		s := f.coll.LatestSnapshot()
		return s != nil && s.CollectedAt.After(after) && ok(s)
	})
}

func snapScans(s *collector.Snapshot, index string) int64 {
	for _, ix := range s.Indexes {
		if ix.IndexRelName == index {
			return ix.IdxScan
		}
	}
	return -1
}

// openUnused lists the open unused_index findings of the two indexes and
// the live drop recommendations for them.
func (f *resetFixture) openUnused(t *testing.T) ([]string, int) {
	t.Helper()
	ctx := context.Background()
	rows, err := f.pool.Query(ctx, `SELECT object_identifier FROM sage.findings
		WHERE category = 'unused_index' AND status = 'open'
		  AND object_identifier IN ('public.ix_seen', 'public.ix_unseen')`)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	var open []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		open = append(open, id)
	}
	rows.Close()
	sort.Strings(open)
	var recs int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM sage.recommendation
		WHERE category = 'unused_index'
		  AND state NOT IN ('superseded', 'abandoned', 'reverted', 'verified', 'inconclusive')`).
		Scan(&recs); err != nil {
		t.Fatalf("recommendations: %v", err)
	}
	return open, recs
}

// Integration against real statistics: ix_seen is scanned and the
// collector sees it; ix_unseen has been unused for six days of analyzer
// time, is then scanned and the counters are reset (pg_stat_reset())
// between two samples, so no snapshot ever shows its scans. Afterwards
// both show zero scans. Neither may be recommended for DROP until a full
// unused window has passed after the reset; then both are.
func TestUnusedIndex_ResetBetweenSamplesNeverYieldsEarlyDrop(t *testing.T) {
	f := newResetFixture(t)
	ctx := context.Background()
	f.scanIndex(t, "ix_seen", "a")
	f.waitSnapshot(t, "ix_seen scanned in a snapshot", func(s *collector.Snapshot) bool {
		return snapScans(s, "ix_seen") > 0 && snapScans(s, "ix_unseen") == 0
	})
	f.now = time.Now().Add(-6 * day)
	f.a.cycle(ctx)

	f.stop() // no sample between the scans of ix_unseen and the reset
	f.scanIndex(t, "ix_unseen", "b")
	var reset time.Time
	if _, err := f.pool.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("pg_stat_reset: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT stats_reset FROM pg_stat_database
		WHERE datname = current_database()`).Scan(&reset); err != nil {
		t.Fatalf("stats_reset: %v", err)
	}
	f.startCollector(t)
	for _, at := range []time.Duration{time.Minute, 2 * day, 7*day - time.Minute} {
		f.waitSnapshot(t, "a zero-scan sample after the reset", func(s *collector.Snapshot) bool {
			return !s.System.RelationStatsEpoch.Before(reset) &&
				snapScans(s, "ix_seen") == 0 && snapScans(s, "ix_unseen") == 0
		})
		f.now = reset.Add(at)
		f.a.cycle(ctx)
		if open, recs := f.openUnused(t); len(open) != 0 || recs != 0 {
			t.Fatalf("reset+%s: open findings %v, %d drop recommendations; want none "+
				"before a full window after the reset", at, open, recs)
		}
	}
	f.waitSnapshot(t, "another sample", func(*collector.Snapshot) bool { return true })
	f.now = reset.Add(7*day + 2*time.Minute)
	f.a.cycle(ctx)
	open, _ := f.openUnused(t)
	if len(open) != 2 || open[0] != "public.ix_seen" || open[1] != "public.ix_unseen" {
		t.Fatalf("after a full clean window: open findings %v, want both indexes", open)
	}
}
