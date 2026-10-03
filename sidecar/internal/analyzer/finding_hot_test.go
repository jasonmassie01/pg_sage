package analyzer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Perf storage phase (dogfood lifeos): 11,022 findings updates, 0 HOT. The
// analyzer refreshes last_seen and the visible fields of every open
// finding each cycle, and last_seen was indexed. With no index on the
// refreshed columns and room left on the page (fillfactor), every refresh
// is a heap-only update: no index entries written, nothing for vacuum.
func TestFindingRefreshIsAHeapOnlyUpdate(t *testing.T) {
	shared := phase2Pool(t)
	testdb.RequireServerVersion(t, shared, 150000, "pg_stat_force_next_flush")
	ctx := context.Background()
	// One connection: the backend that updates is the one whose pending
	// statistics are flushed before the counters are read.
	cfg := shared.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	phase2CleanFindings(t, shared)
	t.Cleanup(func() { phase2CleanFindings(t, shared) })
	f := Finding{Category: "hot_refresh_test", Severity: "warning", ObjectType: "table",
		ObjectIdentifier: "public.hot_refresh", Title: "t", Recommendation: "r",
		Detail: map[string]any{"n": 1}}
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatalf("open: %v", err)
	}
	before := findingUpdateCounters(t, ctx, pool)
	for i := 2; i <= 6; i++ {
		f.Detail = map[string]any{"n": i} // severity unchanged
		if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	after := findingUpdateCounters(t, ctx, pool)
	upd, hot := after[0]-before[0], after[1]-before[1]
	if upd < 5 || hot != upd {
		t.Fatalf("5 refreshes: %d updates, %d HOT; want every refresh HOT", upd, hot)
	}
}

// findingUpdateCounters flushes the connection's pending statistics, then
// returns sage.findings' n_tup_upd and n_tup_hot_upd.
func findingUpdateCounters(t *testing.T, ctx context.Context, pool *pgxpool.Pool) [2]int64 {
	t.Helper()
	if _, err := pool.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
		t.Fatalf("flush: %v", err)
	}
	var c [2]int64
	if err := pool.QueryRow(ctx, `SELECT n_tup_upd, n_tup_hot_upd FROM pg_stat_user_tables
		WHERE schemaname = 'sage' AND relname = 'findings'`).Scan(&c[0], &c[1]); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	return c
}
