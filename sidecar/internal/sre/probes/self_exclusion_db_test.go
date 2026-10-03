package probes

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// Self-exclusion of the SRE probes (perf v1.8.3, perf-selfexcl). The
// probes are evidence for hypotheses and for the cancel action's target:
// a pg_sage session waiting for a lock is not a blocked application
// session, pg_sage's own transactions and active sessions are not the
// workload, and its statements are not the ones that spilled. pg_sage as
// a blocker or xmin holder stays visible (it is protected from actions);
// that is not tested here.

func TestLockChainsProbeCountsApplicationWaitersOnly(t *testing.T) {
	pool, ctx := livePool(t)
	_, app, _ := selfload.Start(t, testdb.SkipUnlessLive(t))
	roots, err := LockRoots(run(ctx, pool, LockChains))
	if err != nil {
		t.Fatalf("lock_chains: %v", err)
	}
	for _, r := range roots {
		if r.PID == app.Holder {
			if r.TotalBlocked != 1 {
				t.Fatalf("application holder blocks %d, want 1 (the application waiter)",
					r.TotalBlocked)
			}
			return
		}
	}
	t.Fatalf("no root for the application holder %d in %+v", app.Holder, roots)
}

func TestLockGraphProbeListsApplicationWaitersOnly(t *testing.T) {
	pool, ctx := livePool(t)
	_, app, sage := selfload.Start(t, testdb.SkipUnlessLive(t))
	edges, err := LockEdges(run(ctx, pool, LockGraph))
	if err != nil {
		t.Fatalf("lock_graph: %v", err)
	}
	var appEdge bool
	for _, e := range edges {
		if e.WaiterPID == sage.Waiter {
			t.Errorf("pg_sage waiter %d is an edge of the lock graph", e.WaiterPID)
		}
		appEdge = appEdge || (e.WaiterPID == app.Waiter && e.BlockerPID == app.Holder)
	}
	if !appEdge {
		t.Fatalf("application edge %d -> %d missing from %+v", app.Waiter, app.Holder, edges)
	}
}

func TestLongTransactionsProbeListsApplicationSessionsOnly(t *testing.T) {
	pool, ctx := livePool(t)
	_, app, sage := selfload.Start(t, testdb.SkipUnlessLive(t))
	xacts, err := LongXacts(run(ctx, pool, LongTransactions))
	if err != nil {
		t.Fatalf("long_transactions: %v", err)
	}
	seen := map[int]bool{}
	for _, x := range xacts {
		seen[x.PID] = true
	}
	for _, pid := range sage.Pids() {
		if seen[pid] {
			t.Errorf("pg_sage session %d listed as a long transaction", pid)
		}
	}
	for _, pid := range app.Pids() {
		if !seen[pid] {
			t.Errorf("application session %d missing from long transactions", pid)
		}
	}
}

func TestLWLockWaitsProbeCountsApplicationBackendsOnly(t *testing.T) {
	pool, ctx := livePool(t)
	selfload.Start(t, testdb.SkipUnlessLive(t))
	groups, err := WaitGroups(run(ctx, pool, LWLockWaits))
	if err != nil {
		t.Fatalf("lwlock_waits: %v", err)
	}
	var here int64
	for _, g := range groups {
		if g.InCurrentDatabase {
			here += g.Backends
		}
	}
	if here != 2 {
		t.Fatalf("active backends of this database = %d in %+v, want 2 (the application "+
			"waiter and sleeper)", here, groups)
	}
}

func TestTempSpillProbeLeavesOutPgSageStatements(t *testing.T) {
	pool, ctx := livePool(t)
	if err := pgssReady(ctx, pool); err != nil {
		t.Skipf("pg_stat_statements unavailable: %v", err)
	}
	sage := selfload.SagePool(t, testdb.SkipUnlessLive(t))
	// Another package's unscoped pg_stat_statements_reset() can clear the
	// entries between the spill and the probe: repeat then.
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		spill(t, ctx, pool, `SELECT count(*) FROM (SELECT g, g + 1 AS h
			FROM generate_series(1, 100000) g ORDER BY md5(g::text)) s`)
		spill(t, ctx, sage, `SELECT count(*) FROM (SELECT g, g + 2, g + 3
			FROM generate_series(1, 100000) g ORDER BY md5(g::text) DESC) s`)
		appID := queryIDLike(ctx, pool, "%AS h%generate_series%")
		sageID := queryIDLike(ctx, pool, "%md5(g::text) DESC%")
		ss, err := SpillStatements(run(ctx, pool, TempSpillStatements))
		if err != nil {
			t.Fatalf("temp_spill_statements: %v", err)
		}
		if appID == 0 || sageID == 0 {
			return []string{fmt.Sprintf("spilling statements not in pg_stat_statements "+
				"(app %d, pg_sage %d)", appID, sageID)}
		}
		var sawApp bool
		for _, s := range ss {
			if s.QueryID == sageID {
				t.Fatalf("pg_sage's spilling statement %d is in the probe", sageID)
			}
			sawApp = sawApp || s.QueryID == appID
		}
		if !sawApp {
			return []string{fmt.Sprintf("application spill %d missing from %+v", appID, ss)}
		}
		return nil
	})
}

// spill runs sql with work_mem 64kB on one session of pool (separate
// statements: a multi-statement string is tagged on its first only).
func spill(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, s := range []string{"SET work_mem = '64kB'", sql, "RESET work_mem"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("spilling statement %q: %v", s, err)
		}
	}
}

// queryIDLike is the queryid of this database's statement matching like
// (0 when there is none).
func queryIDLike(ctx context.Context, pool *pgxpool.Pool, like string) int64 {
	var id int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(max(queryid), 0) FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query LIKE $1`, like).Scan(&id)
	return id
}
