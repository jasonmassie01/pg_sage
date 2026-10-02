package causal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Probes -> causal graph against real PostgreSQL conditions: an
// idle-in-transaction holder, DDL queued behind an active long
// transaction, hot-row contention, and a plan flip caused by a changed
// index.

func livePool(t *testing.T) (*pgxpool.Pool, context.Context, string) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx, dsn
}

func dial(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func uniqueTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	ddl string) string {
	t.Helper()
	name := fmt.Sprintf("sre_causal_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf(ddl, ident)); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident)
	})
	return ident
}

func waitWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not reach %d lock waiters", want)
}

// session runs sql on its own connection in the background (simple
// protocol, so it may hold several statements) and is terminated at
// cleanup.
type session struct {
	conn *pgx.Conn
	pid  int
	done chan struct{}
}

func startSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	dsn, sql string) *session {
	t.Helper()
	s := &session{conn: dial(t, ctx, dsn), done: make(chan struct{})}
	_ = s.conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&s.pid)
	go func() {
		defer close(s.done)
		_, _ = s.conn.PgConn().Exec(context.Background(), sql).ReadAll()
	}()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_terminate_backend($1)", s.pid)
		<-s.done
		_ = s.conn.Close(context.Background())
	})
	return s
}

func lockObservations(ctx context.Context, pool *pgxpool.Pool) []Observation {
	r := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	var out []Observation
	for i, id := range []probes.ID{probes.LockGraph, probes.PreparedXacts} {
		out = append(out, Observation{EvidenceID: fmt.Sprintf("P%d", i+1),
			Result: r.Run(ctx, id, probes.Args{})})
	}
	return out
}

func TestIntegration_IdleInTransactionHolder(t *testing.T) {
	pool, ctx, dsn := livePool(t)
	tbl := uniqueTable(t, ctx, pool, "CREATE TABLE %s (id int)")
	holder := startSession(t, ctx, pool, dsn,
		"BEGIN; SELECT count(*) FROM "+tbl+";")
	time.Sleep(200 * time.Millisecond)
	startSession(t, ctx, pool, dsn, "ALTER TABLE "+tbl+" ADD COLUMN v int")
	waitWaiters(t, ctx, pool, 1)
	startSession(t, ctx, pool, dsn, "SELECT count(*) FROM "+tbl)
	waitWaiters(t, ctx, pool, 2)

	d := DiagnoseLock(lockObservations(ctx, pool), nil)
	root := requireRoot(t, d, IdleInTxHolder, 0.70)
	if root.Subject != fmt.Sprintf("pid %d", holder.pid) {
		t.Fatalf("root subject %q, want holder pid %d", root.Subject, holder.pid)
	}
	if len(d.Contributing) != 1 || d.Contributing[0].Node != DDLLockQueue {
		t.Fatalf("contributing = %+v, want the queued ALTER", d.Contributing)
	}
	requireRuledOut(t, d, HotRowContention, "relation locks")
}

func TestIntegration_DDLQueuedBehindActiveTransaction(t *testing.T) {
	pool, ctx, dsn := livePool(t)
	tbl := uniqueTable(t, ctx, pool, "CREATE TABLE %s (id int); INSERT INTO %[1]s VALUES (1)")
	holder := startSession(t, ctx, pool, dsn,
		"SELECT count(*) FROM "+tbl+", pg_sleep(30)")
	time.Sleep(200 * time.Millisecond)
	startSession(t, ctx, pool, dsn, "ALTER TABLE "+tbl+" ADD COLUMN v int")
	waitWaiters(t, ctx, pool, 1)
	startSession(t, ctx, pool, dsn, "SELECT count(*) FROM "+tbl)
	waitWaiters(t, ctx, pool, 2)

	d := DiagnoseLock(lockObservations(ctx, pool), nil)
	root := requireRoot(t, d, DDLLockQueue, 0.70)
	if root.Subject != fmt.Sprintf("pid %d", holder.pid) {
		t.Fatalf("root subject %q, want the active holder %d", root.Subject, holder.pid)
	}
	requireRuledOut(t, d, IdleInTxHolder, "is active")
}

func TestIntegration_HotRowContention(t *testing.T) {
	pool, ctx, dsn := livePool(t)
	tbl := uniqueTable(t, ctx, pool,
		"CREATE TABLE %s (id int PRIMARY KEY, v int); INSERT INTO %[1]s VALUES (1, 0)")
	update := "UPDATE " + tbl + " SET v = v + 1 WHERE id = 1"
	holder := startSession(t, ctx, pool, dsn,
		"BEGIN; "+update+"; SELECT pg_sleep(30); COMMIT;")
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 3; i++ {
		startSession(t, ctx, pool, dsn, update)
		waitWaiters(t, ctx, pool, i+1)
	}

	d := DiagnoseLock(lockObservations(ctx, pool), nil)
	root := requireRoot(t, d, HotRowContention, 0.70)
	if root.Subject != fmt.Sprintf("pid %d", holder.pid) {
		t.Fatalf("root subject %q, want the row holder %d", root.Subject, holder.pid)
	}
	requireRuledOut(t, d, DDLLockQueue, "ACCESS EXCLUSIVE")
	requireRuledOut(t, d, IdleInTxHolder, "is active")
}

// A plan flip via a changed index: the query uses an index, the index is
// dropped, the plan changes to a sequential scan and the windowed
// latency from pg_stat_statements rises. pg_stat_statements is shared by
// the whole server: another test package can reset it or evict this
// query mid-run, which breaks the cumulative counters the window is
// computed from. Such a run is repeated, never scored.
func TestIntegration_PlanFlipViaDroppedIndex(t *testing.T) {
	pool, ctx, _ := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const attempts = 3
	for i := 1; i <= attempts; i++ {
		if planFlipAttempt(t, ctx, pool) {
			return
		}
		t.Logf("attempt %d: pg_stat_statements was reset or evicted the query "+
			"mid-run; repeating", i)
	}
	t.Fatalf("pg_stat_statements was reset or evicted the query in all %d attempts",
		attempts)
}

// planFlipAttempt runs the scenario once; false means the counters were
// reset under it and the attempt proves nothing.
func planFlipAttempt(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	tbl := uniqueTable(t, ctx, pool, "CREATE TABLE %s (id int, v text); "+
		"INSERT INTO %[1]s SELECT g, md5(g::text) FROM generate_series(1, 200000) g; "+
		"CREATE INDEX ON %[1]s (id); ANALYZE %[1]s")
	query := "SELECT v FROM " + tbl + " WHERE id = $1"
	pf := newPlanFixture(t, ctx, pool, query)
	if pf.reset {
		return false
	}
	pf.capture(t, ctx) // index scan
	pf.sample(t, ctx)
	pf.run(t, ctx, 40)
	pf.sample(t, ctx)
	if _, err := pool.Exec(ctx, "DROP INDEX "+strings.Trim(tbl, `"`)+"_id_idx"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	pf.capture(t, ctx) // sequential scan
	pf.run(t, ctx, 40)
	pf.sample(t, ctx)
	if pf.reset {
		return false
	}
	if pf.hashes[0] == pf.hashes[1] {
		t.Fatalf("dropping the index did not change the plan hash (%s)", pf.hashes[0])
	}
	requirePlanFlip(t, ctx, pool, pf.queryID)
	return true
}

func requirePlanFlip(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queryID int64) {
	t.Helper()
	r := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	ds := DiagnosePlan([]Observation{{EvidenceID: "P1",
		Result: r.Run(ctx, probes.PlanRegressions, probes.Args{})}})
	for _, d := range ds {
		if d.Root != nil && d.Root.Subject == fmt.Sprintf("queryid %d", queryID) {
			if d.Root.Node != PlanFlipRegression || d.Ratio < 1.5 {
				t.Fatalf("flip diagnosis = %+v ratio %.2f", d.Root, d.Ratio)
			}
			requireRuledOut(t, d, SamePlanLatencyRegression, "plan changed")
			return
		}
	}
	t.Fatalf("no plan-flip diagnosis for queryid %d in %+v", queryID, ds)
}

type planFixture struct {
	pool      *pgxpool.Pool
	query     string
	queryID   int64
	hashes    []string
	lastCalls int64
	reset     bool // the counters went missing or backwards mid-run
}

func newPlanFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	query string) *planFixture {
	t.Helper()
	pf := &planFixture{pool: pool, query: query}
	pf.run(t, ctx, 1)
	marker := strings.TrimPrefix(query, "SELECT v FROM ")
	if err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE query LIKE $1 AND dbid = (SELECT oid FROM pg_database
		WHERE datname = current_database()) LIMIT 1`,
		"%"+marker+"%").Scan(&pf.queryID); errors.Is(err, pgx.ErrNoRows) {
		pf.reset = true // reset or evicted before it could be found
		return pf
	} else if err != nil {
		t.Fatalf("queryid: %v", err)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"query_store", "explain_cache"} {
			_, _ = pool.Exec(context.Background(),
				"DELETE FROM sage."+tbl+" WHERE queryid = $1", pf.queryID)
		}
	})
	return pf
}

func (pf *planFixture) run(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var v string
		if err := pf.pool.QueryRow(ctx, pf.query, 1000+i).Scan(&v); err != nil {
			t.Fatalf("run query: %v", err)
		}
	}
}

func (pf *planFixture) capture(t *testing.T, ctx context.Context) {
	t.Helper()
	var plan []byte
	if err := pf.pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+
		strings.Replace(pf.query, "$1", "1000", 1)).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	hash, err := planhash.Compute(plan)
	if err != nil {
		t.Fatalf("plan hash: %v", err)
	}
	pf.hashes = append(pf.hashes, hash)
	if _, err := pf.pool.Exec(ctx, `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, plan_hash)
		VALUES ($1, 'fixture', $2, 'test', $3)`, pf.queryID, plan, hash); err != nil {
		t.Fatalf("store plan: %v", err)
	}
}

// sample records one query_store row from the live pg_stat_statements
// counters, the way the collector does.
func (pf *planFixture) sample(t *testing.T, ctx context.Context) {
	t.Helper()
	var calls *int64
	var total *float64
	if err := pf.pool.QueryRow(ctx, `SELECT sum(calls)::int8,
		sum(total_exec_time)::float8 FROM pg_stat_statements
		WHERE queryid = $1`, pf.queryID).Scan(&calls, &total); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if calls == nil || total == nil || *calls < pf.lastCalls {
		pf.reset = true
		return
	}
	pf.lastCalls = *calls
	if err := querystore.Record(ctx, pf.pool, []querystore.Sample{{
		QueryID: pf.queryID, Calls: *calls, TotalExecMs: *total,
		MeanExecMs: *total / float64(*calls)}}); err != nil {
		t.Fatalf("record sample: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // distinct captured_at per sample
}
