package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func singleConnPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse fixture DSN: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create single-connection pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// G1-B05 / R10: pg_stat_statements keeps one row per (userid, dbid,
// queryid, toplevel). The collector must aggregate to one row per queryid
// so query_store never receives two samples with the same captured_at.
func TestCollectQueries_AggregatesAcrossUsers(t *testing.T) {
	pool := singleConnPool(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", os.Getpid())
	roleA, roleB := "b05_role_a_"+suffix, "b05_role_b_"+suffix
	mustExec(t, pool, `CREATE TABLE IF NOT EXISTS b05_target (id int)`)
	mustExec(t, pool, `INSERT INTO b05_target SELECT generate_series(1, 10)`)
	for _, role := range []string{roleA, roleB} {
		mustExec(t, pool, `CREATE ROLE `+role)
		mustExec(t, pool, `GRANT SELECT ON b05_target TO `+role)
	}
	t.Cleanup(func() {
		for _, role := range []string{roleA, roleB} {
			_, _ = pool.Exec(ctx, `RESET ROLE`)
			_, _ = pool.Exec(ctx, `REVOKE ALL ON b05_target FROM `+role)
			_, _ = pool.Exec(ctx, `DROP ROLE IF EXISTS `+role)
		}
	})
	const q = `SELECT count(*) FROM b05_target WHERE id > 0`
	for role, times := range map[string]int{roleA: 2, roleB: 3} {
		mustExec(t, pool, `SET ROLE `+role)
		for i := 0; i < times; i++ {
			mustExec(t, pool, q)
		}
		mustExec(t, pool, `RESET ROLE`)
	}
	var perUserRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_statements
		WHERE query LIKE '%b05_target WHERE id >%'
		  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`,
	).Scan(&perUserRows); err != nil {
		t.Fatalf("count pg_stat_statements rows: %v", err)
	}
	if perUserRows < 2 {
		t.Fatalf("fixture produced %d per-user rows; need >= 2", perUserRows)
	}
	cfg := testConfig()
	cfg.Collector.MaxQueries = 1000
	queries, err := New(pool, cfg, 170000, noopLog).collectQueries(ctx)
	if err != nil {
		t.Fatalf("collectQueries: %v", err)
	}
	var matches []QueryStats
	for _, qs := range queries {
		if strings.Contains(qs.Query, "b05_target WHERE id >") {
			matches = append(matches, qs)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("got %d rows for one queryid, want 1 aggregated row", len(matches))
	}
	if matches[0].Calls != 5 {
		t.Errorf("aggregated calls = %d, want 5 (2 + 3)", matches[0].Calls)
	}
	wantMean := matches[0].TotalExecTime / float64(matches[0].Calls)
	if math.Abs(matches[0].MeanExecTime-wantMean) > 1e-9 {
		t.Errorf("mean %v != total/calls %v", matches[0].MeanExecTime, wantMean)
	}
}

// G1-B08 / C01: cache_hit_ratio is a fraction in [0,1] computed by the
// real collector expression; empty stats are unknown (NULL), not 0.
func TestCacheHitRatioExpression_FractionAndUnknown(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		hit, read int64
		want      *float64
	}{
		{0, 0, nil},
		{0, 100, ptrF(0)},
		{800, 200, ptrF(0.80)},
		{940, 60, ptrF(0.94)},
		{950, 50, ptrF(0.95)},
		{990, 10, ptrF(0.99)},
	}
	for _, tc := range cases {
		sql := `SELECT (SELECT ` + cacheHitRatioExpr + ` FROM (VALUES ($1::bigint, $2::bigint))
			AS pg_stat_database(blks_hit, blks_read))::float8`
		var got *float64
		if err := pool.QueryRow(context.Background(), sql, tc.hit, tc.read).Scan(&got); err != nil {
			t.Fatalf("evaluate cache hit expression: %v", err)
		}
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("hit=%d read=%d: got %v, want NULL (unknown)", tc.hit, tc.read, *got)
		case tc.want != nil && got == nil:
			t.Errorf("hit=%d read=%d: got NULL, want %v", tc.hit, tc.read, *tc.want)
		case tc.want != nil && math.Abs(*got-*tc.want) > 1e-9:
			t.Errorf("hit=%d read=%d: got %v, want %v", tc.hit, tc.read, *got, *tc.want)
		}
	}
}

func ptrF(v float64) *float64 { return &v }

// G1-B08: the live collector emits a fraction (or the -1 unknown sentinel).
func TestCollectSystem_CacheHitRatioIsFraction(t *testing.T) {
	pool := testPool(t)
	c := New(pool, testConfig(), 170000, noopLog)
	s, err := c.collectSystem(context.Background())
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	if s.CacheHitRatio != CacheHitRatioUnknown &&
		(s.CacheHitRatio < 0 || s.CacheHitRatio > 1) {
		t.Fatalf("cache_hit_ratio = %v, want fraction in [0,1] or unknown", s.CacheHitRatio)
	}
}

// G1-B08: unknown persists as JSON null (so SQL avg() ignores it) and
// round-trips back to the unknown sentinel; known values stay fractions.
func TestSystemStats_CacheHitRatioJSONContract(t *testing.T) {
	raw, err := json.Marshal(SystemStats{CacheHitRatio: CacheHitRatioUnknown})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cache_hit_ratio":null`) {
		t.Fatalf("unknown ratio marshaled as %s; want null", raw)
	}
	var back SystemStats
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.CacheHitRatio != CacheHitRatioUnknown {
		t.Errorf("null round-tripped to %v, want unknown sentinel", back.CacheHitRatio)
	}
	raw, _ = json.Marshal(SystemStats{CacheHitRatio: 0.8, MaxConnections: 7})
	if !strings.Contains(string(raw), `"cache_hit_ratio":0.8`) ||
		!strings.Contains(string(raw), `"max_connections":7`) {
		t.Fatalf("known ratio marshaled as %s", raw)
	}
}

// G1-B16: per-query block I/O time must come from pg_stat_statements
// (PG17 renamed blk_read_time to shared_/local_blk_read_time), not a
// hard-coded zero.
func TestCollectQueries_BlockReadTimeFromStatements(t *testing.T) {
	pool := singleConnPool(t)
	ctx := context.Background()
	mustExec(t, pool, `SET track_io_timing = on`)
	mustExec(t, pool, `SET temp_buffers = '800kB'`)
	mustExec(t, pool, `CREATE TEMP TABLE b16_tmp AS
		SELECT g AS id, repeat('x', 200) AS pad FROM generate_series(1, 60000) g`)
	for i := 0; i < 3; i++ {
		mustExec(t, pool, `SELECT count(*) FROM b16_tmp WHERE pad <> ''`)
	}
	// PG17 split blk_read_time into shared_/local_blk_read_time.
	readTime := "blk_read_time"
	var version int
	if err := pool.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version >= 170000 {
		readTime = "shared_blk_read_time + local_blk_read_time"
	}
	if version >= 160000 && version < 170000 {
		// Measured: PG16 reads the temp table's local blocks with 0 ms
		// blk_read_time; PG15 and PG17 time them.
		t.Skip("PG16 pg_stat_statements does not time local-buffer reads; " +
			"this fixture reads a temp table")
	}
	var want float64
	err := pool.QueryRow(ctx, `SELECT COALESCE(sum(`+readTime+`), 0)
		FROM pg_stat_statements
		WHERE query LIKE '%FROM b16_tmp WHERE pad%'`).Scan(&want)
	if err != nil {
		t.Fatalf("read expected block time: %v", err)
	}
	if want <= 0 {
		t.Fatalf("workload produced no block read time (%v); fixture invalid", want)
	}
	cfg := testConfig()
	cfg.Collector.MaxQueries = 1000
	queries, err := New(pool, cfg, 170000, noopLog).collectQueries(ctx)
	if err != nil {
		t.Fatalf("collectQueries: %v", err)
	}
	for _, q := range queries {
		if strings.Contains(q.Query, "FROM b16_tmp WHERE pad") {
			if math.Abs(q.BlkReadTime-want) > 1e-6 {
				t.Fatalf("BlkReadTime = %v, want %v", q.BlkReadTime, want)
			}
			return
		}
	}
	t.Fatal("b16 workload query not collected")
}
