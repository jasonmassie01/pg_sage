package autonomy

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-04 on real PostgreSQL: the freeze custodian's bloat plans come
// from the catalog estimate on tables at the size floor, never from the
// dead-tuple ratio, and the optional pgstattuple confirmation is bounded.

func bloatTestExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// bloatedTable creates bloatfx.table with rows rows, keeps keepTenths of
// every ten ids, vacuums (the deleted rows become free space, the pages
// stay) and analyzes. keepTenths=10 keeps every row.
func bloatedTable(t *testing.T, pool *pgxpool.Pool, table string, rows, keepTenths int) {
	t.Helper()
	bloatTestExec(t, pool, fmt.Sprintf(`DROP TABLE IF EXISTS bloatfx.%[1]s;
		CREATE TABLE bloatfx.%[1]s (id bigint, payload text)
		WITH (autovacuum_enabled = false);
		INSERT INTO bloatfx.%[1]s SELECT g, md5(g::text) FROM generate_series(1, %[2]d) g;
		DELETE FROM bloatfx.%[1]s WHERE id %% 10 >= %[3]d`, table, rows, keepTenths))
	bloatTestExec(t, pool, "VACUUM bloatfx."+table)
	bloatTestExec(t, pool, "ANALYZE bloatfx."+table)
}

func bloatSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	bloatTestExec(t, pool, "DROP SCHEMA IF EXISTS bloatfx CASCADE; CREATE SCHEMA bloatfx")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS bloatfx CASCADE")
	})
}

func proposalsFor(proposals []Proposal, target string) []Proposal {
	var out []Proposal
	for _, proposal := range proposals {
		if len(proposal.TargetObjects) == 1 && proposal.TargetObjects[0] == target {
			out = append(out, proposal)
		}
	}
	return out
}

// The owner's lifeos_runtime_schema_meta case: 2 row versions, 1 dead
// (dead ratio 0.5), analyzed.
func TestFreezeBloatTwoRowTableWithADeadRowHasNoProposal(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	bloatSchema(t, pool)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// Each step's counts are flushed before the next, so ANALYZE's live
	// count is not added to the insert's still-pending one.
	for _, sql := range []string{
		`CREATE TABLE bloatfx.schema_meta (k text, v text) WITH (autovacuum_enabled = false);
		INSERT INTO bloatfx.schema_meta VALUES ('a', '1'), ('b', '2')`,
		"ANALYZE bloatfx.schema_meta",
		"DELETE FROM bloatfx.schema_meta WHERE k = 'b'",
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if err := testdb.FlushStats(ctx, conn); err != nil {
			t.Fatal(err)
		}
	}
	var live, dead int64
	if err := pool.QueryRow(ctx, `SELECT pg_stat_get_live_tuples(c.oid),
		pg_stat_get_dead_tuples(c.oid) FROM pg_class c
		WHERE c.oid = 'bloatfx.schema_meta'::regclass`).Scan(&live, &dead); err != nil ||
		live != 1 || dead != 1 {
		t.Fatalf("fixture tuples live=%d dead=%d (%v), want 1 and 1 (dead ratio 0.5)",
			live, dead, err)
	}
	for _, floor := range []int64{0, 1} { // shipped floor, and no floor at all
		custodian := NewPostgresFreezeCustodian(pool, "testdb", 25).
			WithBloatBounds(floor, catalogread.Default())
		proposals, err := custodian.Scan(ctx)
		if err != nil {
			t.Fatalf("Scan (floor %d): %v", floor, err)
		}
		if got := proposalsFor(proposals, "bloatfx.schema_meta"); len(got) != 0 {
			t.Fatalf("floor %d: 2-row table with 1 dead row proposed %#v", floor, got)
		}
	}
}

func TestFreezeBloatNeverAnalyzedTableHasNoProposal(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	bloatSchema(t, pool)
	bloatTestExec(t, pool, `CREATE TABLE bloatfx.fresh (id bigint, payload text)
		WITH (autovacuum_enabled = false);
		INSERT INTO bloatfx.fresh SELECT g, md5(g::text) FROM generate_series(1, 20000) g;
		DELETE FROM bloatfx.fresh WHERE id % 10 < 8`)
	if err := testdb.FlushStats(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var reltuples float64
	if err := pool.QueryRow(ctx, `SELECT reltuples FROM pg_class
		WHERE oid = 'bloatfx.fresh'::regclass`).Scan(&reltuples); err != nil || reltuples >= 0 {
		t.Fatalf("fixture reltuples = %v (%v), want -1 (never analyzed)", reltuples, err)
	}
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 25).
		WithBloatBounds(1, catalogread.Default())
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := proposalsFor(proposals, "bloatfx.fresh"); len(got) != 0 {
		t.Fatalf("never-analyzed table with 80%% dead rows proposed %#v", got)
	}
}

func TestFreezeBloatEstimateMatchesMeasuredBloat(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	bloatSchema(t, pool)
	bloatedTable(t, pool, "bloated", 40000, 3) // 70% of the rows removed
	bloatedTable(t, pool, "healthy", 40000, 10)
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 25).
		WithBloatBounds(64*8192, catalogread.Default())
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := proposalsFor(proposals, "bloatfx.healthy"); len(got) != 0 {
		t.Fatalf("healthy table proposed %#v", got)
	}
	got := proposalsFor(proposals, "bloatfx.bloated")
	if len(got) != 1 || got[0].ResponseKind != "park_bloat" ||
		got[0].Feature != "bloat_remediation" || got[0].SQL != "" {
		t.Fatalf("bloated table proposals = %#v", got)
	}
	estimate, _ := got[0].Evidence["bloat_ratio"].(float64)
	if estimate < 0.6 || estimate > 0.8 || got[0].Evidence["bloat_source"] != "estimate" {
		t.Fatalf("estimate for a 70%%-deleted table = %v (%#v)", estimate, got[0].Evidence)
	}
}

func TestFreezeBloatConfirmationIsBounded(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	bloatSchema(t, pool)
	bloatTestExec(t, pool, "CREATE EXTENSION IF NOT EXISTS pgstattuple")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP EXTENSION IF EXISTS pgstattuple")
	})
	small, large := map[int64]string{}, map[int64]string{}
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("small_%02d", i)
		bloatedTable(t, pool, name, 900, 3) // ~8 pages
		small[relationOID(t, pool, "bloatfx."+name)] = name
	}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("large_%d", i)
		bloatedTable(t, pool, name, 7200+i*900, 3) // ~64-72 pages
		large[relationOID(t, pool, "bloatfx."+name)] = name
	}
	if _, err := testdb.ResetStatements(ctx, pool); err != nil {
		t.Fatalf("reset this database's pg_stat_statements: %v", err)
	}
	recorder := &testdb.QueryRecorder{}
	traced := recordedPool(t, pool, recorder)
	custodian := NewPostgresFreezeCustodian(traced, "testdb", 25).
		WithBloatBounds(32*8192, catalogread.Default())
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	probes := recorder.Matching("pgstattuple_approx")
	if len(probes) != 3 {
		t.Fatalf("pgstattuple_approx calls = %d, want 3 (5 candidates, 3 per cycle)",
			len(probes))
	}
	for _, probe := range probes {
		oid, _ := probe.Args[0].(int64)
		if _, under := small[oid]; under {
			t.Fatalf("probed %s under the size floor", small[oid])
		}
	}
	for _, scan := range recorder.Matching("relfrozenxid") {
		if strings.Contains(scan.SQL, "pgstattuple") ||
			strings.Contains(scan.SQL, "pg_relation_size") {
			t.Fatalf("the per-cycle scan opens relations:\n%s", scan.SQL)
		}
	}
	assertServerProbeCalls(t, pool, 3)
	assertBoundedProposals(t, proposals, small, large)
}

func assertServerProbeCalls(t *testing.T, pool *pgxpool.Pool, limit int64) {
	t.Helper()
	var calls int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(calls), 0)::bigint
		FROM pg_stat_statements WHERE query ILIKE '%pgstattuple_approx%'
		AND query NOT ILIKE '%pg_stat_statements%'
		AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`).
		Scan(&calls); err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	if calls < 1 || calls > limit {
		t.Fatalf("server saw %d pgstattuple_approx calls, want 1..%d", calls, limit)
	}
}

func assertBoundedProposals(t *testing.T, proposals []Proposal,
	small, large map[int64]string) {
	t.Helper()
	for _, name := range small {
		if got := proposalsFor(proposals, "bloatfx."+name); len(got) != 0 {
			t.Fatalf("table %s under the size floor proposed %#v", name, got)
		}
	}
	sources := map[string]int{}
	for _, name := range large {
		got := proposalsFor(proposals, "bloatfx."+name)
		if len(got) != 1 || got[0].ResponseKind != "park_bloat" {
			t.Fatalf("large bloated table %s proposals = %#v", name, got)
		}
		source, _ := got[0].Evidence["bloat_source"].(string)
		sources[source]++
	}
	if sources["pgstattuple"] != 3 || sources["estimate"] != 2 {
		t.Fatalf("bloat sources = %v, want 3 confirmed and 2 estimated", sources)
	}
}

func TestFreezeBloatConfirmationLockTimeoutKeepsEstimate(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	bloatSchema(t, pool)
	bloatTestExec(t, pool, "CREATE EXTENSION IF NOT EXISTS pgstattuple")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP EXTENSION IF EXISTS pgstattuple")
	})
	bloatedTable(t, pool, "locked", 9000, 3)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx,
		"LOCK TABLE bloatfx.locked IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 25).WithBloatBounds(32*8192,
		catalogread.Timeouts{Statement: 5 * time.Second, Lock: 100 * time.Millisecond})
	started := time.Now()
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("a confirmation timeout failed the scan: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("scan waited %v on the locked table", elapsed)
	}
	got := proposalsFor(proposals, "bloatfx.locked")
	if len(got) != 1 || got[0].Evidence["bloat_source"] != "estimate" {
		t.Fatalf("locked table proposals = %#v", got)
	}
	detail, _ := got[0].Evidence["bloat_confirm_error"].(string)
	estimate, _ := got[0].Evidence["bloat_ratio"].(float64)
	if !strings.Contains(detail, "55P03") || math.IsNaN(estimate) || estimate < 0.5 {
		t.Fatalf("confirmation error %q, estimate %v", detail, estimate)
	}
}

func relationOID(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var oid int64
	if err := pool.QueryRow(context.Background(),
		"SELECT $1::regclass::oid::bigint", name).Scan(&oid); err != nil {
		t.Fatalf("oid of %s: %v", name, err)
	}
	return oid
}

func recordedPool(t *testing.T, pool *pgxpool.Pool,
	recorder *testdb.QueryRecorder) *pgxpool.Pool {
	t.Helper()
	config := pool.Config().Copy()
	config.ConnConfig.Tracer = recorder
	traced, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	return traced
}

var _ pgx.QueryTracer = (*testdb.QueryRecorder)(nil)
