package cost

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/cost"))
}

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_DATABASE_URL not set; skipping cost integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("cannot connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot ping: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// statementSamples reads this database's pg_stat_statements counters as
// the collector would, with the statistics epoch.
func statementSamples(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []querystore.Sample {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT s.queryid, sum(s.calls)::int8,
		sum(s.total_exec_time)::float8, (SELECT stats_reset FROM pg_stat_statements_info)
		FROM pg_stat_statements s
		WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND s.queryid IS NOT NULL
		GROUP BY s.queryid`)
	if err != nil {
		t.Skipf("pg_stat_statements not available: %v", err)
	}
	defer rows.Close()
	var out []querystore.Sample
	for rows.Next() {
		var s querystore.Sample
		if err := rows.Scan(&s.QueryID, &s.Calls, &s.TotalExecMs, &s.StatsEpoch); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s.Calls > 0 {
			s.MeanExecMs = s.TotalExecMs / float64(s.Calls)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func runWorkload(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var v int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM generate_series(1, $1::int)`,
			20_000+i).Scan(&v); err != nil {
			t.Fatalf("workload: %v", err)
		}
		const md5SQL = `SELECT md5(g::text) FROM generate_series(1, 5000) g`
		if _, err := pool.Exec(ctx, md5SQL); err != nil {
			t.Fatalf("workload: %v", err)
		}
	}
}

// Integration: real pg_stat_statements deltas through the recorder give
// query shares that sum to 1 within 1e-9. It runs on the exclusive server:
// pg_stat_statements is cluster-wide, and other packages filling it on the
// shared server evict this window's fresh entries (market-04b).
func TestAttribution_RealStatementDeltasSumToOne(t *testing.T) {
	pool, ctx := requireExclusiveDB(t, "pg_stat_statements capacity")
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		t.Skipf("pg_stat_statements extension: %v", err)
	}
	w, err := measureWindow(t, ctx, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	deltas := w.deltas
	if len(deltas) < 2 || w.dropped != 0 {
		t.Fatalf("%d deltas (%d dropped), want the workload's queries", len(deltas), w.dropped)
	}
	a := Attribute(1.25, time.Hour, 2, deltas)
	var sum float64
	for _, q := range a.Queries {
		if q.Share < 0 || q.Cost < 0 || q.Database != "orders" {
			t.Fatalf("query %+v: negative or unlabelled", q)
		}
		sum += q.Share
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("shares sum to %.12f, want 1 +/- 1e-9", sum)
	}
}

func insertStoreRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	at time.Time, calls int64, total float64, epoch *time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
		(captured_at, queryid, calls, total_exec_time, mean_exec_time, stats_epoch)
		VALUES ($1, $2, $3, $4, 0, $5)`, at, qid, calls, total, epoch); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// State transition: a statistics reset in the middle of the window drops
// that pair (never a negative delta) and keeps the pairs on either side.
func TestQueryDeltas_ResetDropsThePair(t *testing.T) {
	pool, ctx := requireDB(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.query_store`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	e1, e2 := base.Add(-24*time.Hour), base.Add(30*time.Minute)
	insertStoreRow(t, ctx, pool, 11, base, 10, 1000, &e1)
	insertStoreRow(t, ctx, pool, 11, base.Add(10*time.Minute), 20, 3000, &e1) // +2000
	insertStoreRow(t, ctx, pool, 11, base.Add(40*time.Minute), 5, 500, &e2)   // reset
	insertStoreRow(t, ctx, pool, 11, base.Add(50*time.Minute), 9, 1500, &e2)  // +1000
	// Same epoch recorded, counters fell (entry evicted): dropped too.
	insertStoreRow(t, ctx, pool, 12, base, 10, 900, &e1)
	insertStoreRow(t, ctx, pool, 12, base.Add(10*time.Minute), 2, 100, &e1)
	// Unknown epoch next to a known one: incomparable.
	insertStoreRow(t, ctx, pool, 13, base, 1, 100, nil)
	insertStoreRow(t, ctx, pool, 13, base.Add(10*time.Minute), 5, 700, &e1)
	deltas, dropped, err := QueryDeltas(ctx, pool, "orders", base.Add(-time.Minute),
		base.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryDeltas: %v", err)
	}
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3 (reset, eviction, unknown epoch)", dropped)
	}
	if len(deltas) != 1 || deltas[0].QueryID != 11 || !near(deltas[0].ExecMs, 3000) {
		t.Fatalf("deltas = %+v, want query 11 with 2000+1000 ms", deltas)
	}
}

func TestQueryDeltas_EmptyStoreIsEmpty(t *testing.T) {
	pool, ctx := requireDB(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.query_store`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	deltas, dropped, err := QueryDeltas(ctx, pool, "orders", time.Now().Add(-time.Hour),
		time.Now())
	if err != nil || len(deltas) != 0 || dropped != 0 {
		t.Fatalf("empty store: %v %d %v, want nothing and no error", deltas, dropped, err)
	}
}

func TestQueryDeltas_ClosedPoolIsAnError(t *testing.T) {
	pool, ctx := requireDB(t)
	dead, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	dead.Close()
	if _, _, err := QueryDeltas(ctx, dead, "orders", time.Now().Add(-time.Hour),
		time.Now()); err == nil {
		t.Fatal("a closed pool must return an error, not an empty attribution")
	}
}
