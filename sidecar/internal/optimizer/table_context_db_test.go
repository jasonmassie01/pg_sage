package optimizer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// indexes-05: the table context used to EXPLAIN every statement of the
// table (GENERIC_PLAN on PG16+, on a raw pool connection with no statement
// or lock timeout) for plans nothing read. It now issues no EXPLAIN.

const tcProbeApp = "tc_plan_probe"

// tcFixture creates schema tc_probe with one table and a pool whose
// sessions carry tcProbeApp, so pg_stat_activity shows only its queries.
func tcFixture(t *testing.T, table string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = tcProbeApp
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	setup := fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS tc_probe;
		DROP TABLE IF EXISTS tc_probe.%[1]s;
		CREATE TABLE tc_probe.%[1]s (id int PRIMARY KEY, v int);
		INSERT INTO tc_probe.%[1]s SELECT g, g %% 97 FROM generate_series(1, 2000) g;
		ANALYZE tc_probe.%[1]s`, table)
	if _, err := pool.Exec(context.Background(), setup); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS tc_probe."+table)
	})
	return pool
}

// tcSnapshot holds the table and n parameterized statements on it; the
// aliases keep them distinct after fingerprint grouping.
func tcSnapshot(table string, n int) *collector.Snapshot {
	snap := &collector.Snapshot{Tables: []collector.TableStats{{SchemaName: "tc_probe",
		RelName: table, NLiveTup: 2000, SeqScan: 10}}}
	for i := 1; i <= n; i++ {
		snap.Queries = append(snap.Queries, collector.QueryStats{QueryID: int64(i),
			Calls: 500, MeanExecTime: 2, TotalExecTime: float64(1000 + i),
			Query: fmt.Sprintf("SELECT id AS c%d FROM tc_probe.%s WHERE v = $1", i, table)})
	}
	return snap
}

// explainCount counts statements in this database's pg_stat_statements
// that EXPLAIN the table (pg_stat_statements tracks utility statements).
func explainCount(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `SELECT coalesce(sum(s.calls), 0)
		FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid AND d.datname = current_database()
		WHERE s.query ILIKE 'EXPLAIN%' AND s.query ILIKE '%' || $1 || '%'`,
		"tc_probe."+table).Scan(&n)
	if err != nil {
		t.Skipf("pg_stat_statements unavailable; EXPLAIN count not verifiable: %v", err)
	}
	return n
}

func tcOptimizer(pool *pgxpool.Pool) *Optimizer {
	return New(pool, &config.OptimizerConfig{MinQueryCalls: 1},
		func(string, string, ...any) {})
}

// Integration, PG 16/17: behind a queued ACCESS EXCLUSIVE lock the context
// builder returns within 2 s, issues no EXPLAIN and leaves no pg_sage
// backend waiting on a lock (failed before: GENERIC_PLAN blocked forever).
func TestTableContext_LockQueueIssuesNoExplain(t *testing.T) {
	pool := tcFixture(t, "locked")
	locker, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin locker: %v", err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(context.Background(),
		"LOCK TABLE tc_probe.locked IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TableContext, 1)
	go func() {
		tc, _, _ := tcOptimizer(pool).TableContext(ctx, tcSnapshot("locked", 3),
			"tc_probe.locked")
		done <- tc
	}()
	var tc TableContext
	select {
	case tc = <-done:
	case <-time.After(2 * time.Second):
		waiting := waitingExplains(t)
		cancel()
		<-done
		t.Fatalf("table context blocked behind the lock for 2s; waiting EXPLAINs: %d",
			waiting)
	}
	if tc.Table != "locked" || len(tc.Queries) != 3 {
		t.Fatalf("context = %+v, want the locked table with its 3 statements", tc)
	}
	if n := waitingExplains(t); n != 0 {
		t.Fatalf("%d pg_sage EXPLAINs left waiting on a lock", n)
	}
}

// waitingExplains counts the probe pool's EXPLAINs waiting on a lock.
func waitingExplains(t *testing.T) int {
	t.Helper()
	admin := connectTestDB(t)
	defer admin.Close()
	var n int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = $1 AND wait_event_type = 'Lock'
		AND query ILIKE 'EXPLAIN%'`, tcProbeApp).Scan(&n); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return n
}

// Boundary: 200 statements on one table cause no plan-capture query; no
// statements cause none either (failed before: one EXPLAIN per statement).
func TestTableContext_ManyStatementsCauseNoPlanCapture(t *testing.T) {
	pool := tcFixture(t, "many")
	before := explainCount(t, pool, "many")
	tc, ok, err := tcOptimizer(pool).TableContext(context.Background(),
		tcSnapshot("many", 200), "tc_probe.many")
	if err != nil || !ok || len(tc.Queries) == 0 || len(tc.Columns) != 2 {
		t.Fatalf("context = %+v (%v, %v), want the table, its statements and columns",
			tc, ok, err)
	}
	if got := explainCount(t, pool, "many") - before; got != 0 {
		t.Fatalf("building the context ran %d EXPLAINs of the table, want 0", got)
	}
	empty, ok, err := tcOptimizer(pool).TableContext(context.Background(),
		tcSnapshot("many", 0), "tc_probe.many")
	if err != nil || !ok || len(empty.Queries) != 0 {
		t.Fatalf("no-statement context = %+v (%v, %v)", empty, ok, err)
	}
	if got := explainCount(t, pool, "many") - before; got != 0 {
		t.Fatalf("a table without statements ran %d EXPLAINs, want 0", got)
	}
}

// Concurrency: two tables' contexts built in parallel each hold only their
// own table's statements (no shared capture state). -race is unavailable
// on this machine; the assertions still catch crossed results.
func TestTableContext_ParallelTablesShareNoState(t *testing.T) {
	pool := tcFixture(t, "left_t")
	_ = tcFixture(t, "right_t")
	snap := tcSnapshot("left_t", 4)
	right := tcSnapshot("right_t", 2)
	for i := range right.Queries {
		right.Queries[i].QueryID += 1000
	}
	snap.Tables = append(snap.Tables, right.Tables...)
	snap.Queries = append(snap.Queries, right.Queries...)
	o := tcOptimizer(pool)
	var wg sync.WaitGroup
	results := make([]TableContext, 2)
	for i, table := range []string{"tc_probe.left_t", "tc_probe.right_t"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tc, ok, err := o.TableContext(context.Background(), snap, table)
			if err != nil || !ok {
				t.Errorf("%s: ok %v err %v", table, ok, err)
			}
			results[i] = tc
		}()
	}
	wg.Wait()
	assertOwnQueries(t, results[0], "left_t", 4, 1, 4)
	assertOwnQueries(t, results[1], "right_t", 2, 1001, 1002)
}

func assertOwnQueries(t *testing.T, tc TableContext, table string, n int, lo, hi int64) {
	t.Helper()
	if tc.Table != table || len(tc.Queries) != n {
		t.Fatalf("%s context = %+v, want %d statements", table, tc, n)
	}
	for _, q := range tc.Queries {
		if q.QueryID < lo || q.QueryID > hi {
			t.Fatalf("%s holds queryid %d of another table", table, q.QueryID)
		}
	}
}

// spPool is a pool whose sessions run with search_path=app, public, the
// way an application on search_path=app (and a pg_sage that shares the
// database's or role's setting) connects. Schema app is created empty.
func spPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "app, public"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS app CASCADE;
		CREATE SCHEMA app`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS app CASCADE")
	})
	return pool
}

const spStatement = "SELECT id, total FROM orders WHERE customer_id = $1"

// spWorkload runs the unqualified statement and returns its
// pg_stat_statements row as the collector reads it.
func spWorkload(t *testing.T, pool *pgxpool.Pool) collector.QueryStats {
	t.Helper()
	ctx := context.Background()
	for i := range 20 {
		if _, err := pool.Exec(ctx, spStatement, i); err != nil {
			t.Fatalf("workload statement: %v", err)
		}
	}
	var q collector.QueryStats
	err := pool.QueryRow(ctx, `SELECT s.queryid, s.query, s.calls, s.total_exec_time,
		s.mean_exec_time FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid AND d.datname = current_database()
		WHERE s.query = $1`, spStatement).Scan(&q.QueryID, &q.Query, &q.Calls,
		&q.TotalExecTime, &q.MeanExecTime)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	if strings.Contains(q.Query, "app.") || q.Calls < 20 {
		t.Fatalf("recorded statement %+v, want the unqualified text with 20 calls", q)
	}
	return q
}

// Integration (indexes-04): statements on app.orders run under
// search_path=app reach app.orders' context, HypoPG verifies an index on
// app.orders, and the admitted recommendation names them as affected.
// Failed before: the context had no statements ("no workload query could
// be measured", unverified, no affected queryids).
func TestAdmit_SearchPathStatementsVerifyAppIndex(t *testing.T) {
	pool := spPool(t)
	ctx := context.Background()
	if !hypopgInstallable(t, pool) {
		t.Skip("HypoPG server extension unavailable; the app index cannot be verified")
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS hypopg WITH SCHEMA app;
		CREATE TABLE app.orders (id int PRIMARY KEY, customer_id int, total int);
		INSERT INTO app.orders SELECT g, g % 5000, g % 97 FROM generate_series(1, 200000) g`,
	); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "VACUUM ANALYZE app.orders"); err != nil {
		t.Fatalf("vacuum fixture: %v", err)
	}
	q := spWorkload(t, pool)
	snap := &collector.Snapshot{Queries: []collector.QueryStats{q},
		Tables: []collector.TableStats{{SchemaName: "app", RelName: "orders",
			NLiveTup: 200000, SeqScan: 20, SeqTupRead: 4000000}}}
	o := New(pool, admissionConfig(), admitNoLog)
	tc, ok, err := o.TableContext(ctx, snap, "app.orders")
	if err != nil || !ok || len(tc.Queries) != 1 || tc.Queries[0].QueryID != q.QueryID {
		t.Fatalf("context of app.orders: queries %+v ok %v err %v, want queryid %d",
			tc.Queries, ok, err, q.QueryID)
	}
	a := o.Admit(ctx, Recommendation{IndexType: "btree", DDL: "CREATE INDEX CONCURRENTLY " +
		"orders_customer_id ON app.orders (customer_id)"}, tc)
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfVerified ||
		a.Rec.WhatIfMeasured != 1 || !slices.Equal(a.Rec.AffectedQueryIDs, []int64{q.QueryID}) {
		t.Fatalf("admission %s (%s): verdict %q measured %d affected %v, want verified "+
			"with queryid %d", a.Outcome, a.Reason, a.Rec.WhatIf, a.Rec.WhatIfMeasured,
			a.Rec.AffectedQueryIDs, q.QueryID)
	}
}

// No state-transition tests: the table context has no modes or states.
