package workload

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("test database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The SQL predicate is the same rule as the Go classifier: on every case,
// PostgreSQL's regex engine must agree with Go's, or a LIMITed reader
// (tuner candidates, verification targets) would pick statements the Go
// filters drop, or the reverse.
func TestDiagnosticSQLMatchesGoOnRealPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, tc := range classifyCases {
		var diagnostic, advisable bool
		err := pool.QueryRow(ctx, `SELECT `+DiagnosticSQL("q")+`, `+AdviceSQL("q")+
			` FROM (SELECT $1::text AS q) s`, tc.query).Scan(&diagnostic, &advisable)
		if err != nil {
			t.Fatalf("%s: evaluate predicates: %v", tc.name, err)
		}
		if diagnostic != IsDiagnostic(tc.query) {
			t.Errorf("%s: DiagnosticSQL = %v, Go IsDiagnostic = %v", tc.name, diagnostic,
				IsDiagnostic(tc.query))
		}
		if tc.want != Workload && tc.want != Self && !diagnostic {
			t.Errorf("%s: DiagnosticSQL misses a %q statement", tc.name, tc.want)
		}
		if advisable != (tc.want == Workload) {
			t.Errorf("%s: AdviceSQL = %v, Go class %q", tc.name, advisable, tc.want)
		}
	}
}

func TestAdviceSQLTreatsNullAsWorkload(t *testing.T) {
	pool := testPool(t)
	var advisable, diagnostic bool
	err := pool.QueryRow(context.Background(), `SELECT `+AdviceSQL("q")+`, `+
		DiagnosticSQL("q")+` FROM (SELECT NULL::text AS q) s`).Scan(&advisable, &diagnostic)
	if err != nil {
		t.Fatalf("evaluate predicates on NULL: %v", err)
	}
	if !advisable || diagnostic {
		t.Fatalf("NULL text: advisable=%v diagnostic=%v, want true/false", advisable,
			diagnostic)
	}
}

// pg_stat_statements stores what clients really send: run diagnostic
// statements for real and read them back through the predicate.
func TestAdviceSQLOnRealPgStatStatements(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var present bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`).Scan(&present); err != nil || !present {
		t.Skip("pg_stat_statements not installed in the test database")
	}
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS workload_probe_t (id int, v text)",
		"EXPLAIN (ANALYZE, BUFFERS) SELECT count(*) FROM workload_probe_t WHERE id > 7",
		"VACUUM workload_probe_t",
		"ANALYZE workload_probe_t",
		"SELECT count(*) FROM workload_probe_t WHERE v = 'workload_probe_marker'",
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS workload_probe_t")
	})
	rows, err := pool.Query(ctx, `SELECT query, `+AdviceSQL("query")+`
		FROM pg_stat_statements WHERE query ILIKE '%workload_probe_t%'
		AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	defer rows.Close()
	seen := map[bool]int{}
	for rows.Next() {
		var q string
		var advisable bool
		if err := rows.Scan(&q, &advisable); err != nil {
			t.Fatal(err)
		}
		if advisable != !Excluded(q) {
			t.Errorf("SQL advisable=%v but Go Excluded=%v for %q", advisable, Excluded(q), q)
		}
		seen[advisable]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen[true] < 1 || seen[false] < 3 {
		t.Fatalf("want the SELECT advisable and EXPLAIN/VACUUM/ANALYZE excluded, got %v",
			seen)
	}
}

// footprint-06 parity: one corpus (the shared classify cases plus the
// third-party monitor statements) is classified identically by the Go
// classifier and the SQL predicate on real PostgreSQL. Before the fix a
// Datadog or pgwatch catalog read was Self in Go and workload in SQL.
func TestClassifyAdviceSQLParity_MonitorCorpus(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	corpus := append(append([]classifyCase{}, classifyCases...), monitorCases...)
	for _, tc := range corpus {
		var advisable bool
		err := pool.QueryRow(ctx, `SELECT `+AdviceSQL("q")+
			` FROM (SELECT $1::text AS q) s`, tc.query).Scan(&advisable)
		if err != nil {
			t.Fatalf("%s: evaluate AdviceSQL: %v", tc.name, err)
		}
		if advisable != !Excluded(tc.query) {
			t.Errorf("%s: AdviceSQL advisable = %v, Go Classify = %q", tc.name, advisable,
				Classify(tc.query))
		}
		if advisable != (tc.want == Workload) {
			t.Errorf("%s: AdviceSQL advisable = %v, want class %q", tc.name, advisable,
				tc.want)
		}
	}
}

// collection-10: CALL and DO stay workload in SQL as in Go, and are not
// diagnostic.
func TestProceduralSQLMatchesGoOnRealPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, tc := range proceduralCases {
		var diagnostic, advisable bool
		err := pool.QueryRow(ctx, `SELECT `+DiagnosticSQL("q")+`, `+AdviceSQL("q")+
			` FROM (SELECT $1::text AS q) s`, tc.query).Scan(&diagnostic, &advisable)
		if err != nil {
			t.Fatalf("%s: evaluate predicates: %v", tc.name, err)
		}
		if diagnostic || !advisable {
			t.Errorf("%s: DiagnosticSQL = %v, AdviceSQL = %v, want false/true", tc.name,
				diagnostic, advisable)
		}
		if advisable != !Excluded(tc.query) {
			t.Errorf("%s: AdviceSQL = %v but Go Excluded = %v", tc.name, advisable,
				Excluded(tc.query))
		}
	}
}

// Utility and DDL statements as pg_stat_statements really stores them
// (track_utility = on) are excluded by the SQL predicate and by Go alike.
func TestUtilityAndDDLOnRealPgStatStatements(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var present bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`).Scan(&present); err != nil || !present {
		t.Skip("pg_stat_statements not installed in the test database")
	}
	runUtilityStatements(t, pool)
	rows, err := pool.Query(ctx, `SELECT query, `+AdviceSQL("query")+`
		FROM pg_stat_statements WHERE (query ILIKE '%workload_util%'
		OR query IN ('BEGIN', 'COMMIT'))
		AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	defer rows.Close()
	seen := map[bool]int{}
	for rows.Next() {
		var q string
		var advisable bool
		if err := rows.Scan(&q, &advisable); err != nil {
			t.Fatal(err)
		}
		if advisable != !Excluded(q) {
			t.Errorf("SQL advisable=%v but Go Excluded=%v for %q", advisable, Excluded(q), q)
		}
		if advisable && !strings.HasPrefix(q, "SELECT") {
			t.Errorf("utility or DDL statement %q is advisable", q)
		}
		seen[advisable]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen[true] < 1 || seen[false] < 7 {
		t.Fatalf("want the SELECT advisable and >= 7 utility/DDL excluded, got %v", seen)
	}
}

// runUtilityStatements runs transaction control, LOCK and DDL on one
// connection so pg_stat_statements tracks them (track_utility = on).
func runUtilityStatements(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS workload_util_t")
	})
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS workload_util_t (id int)",
		"BEGIN", "SAVEPOINT workload_util_sp", "RELEASE SAVEPOINT workload_util_sp",
		"LOCK TABLE workload_util_t IN SHARE MODE", "COMMIT",
		"ALTER TABLE workload_util_t ADD COLUMN IF NOT EXISTS c int",
		"COMMENT ON TABLE workload_util_t IS 'workload_util'",
		"TRUNCATE workload_util_t",
		"SELECT count(*) FROM workload_util_t",
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}
