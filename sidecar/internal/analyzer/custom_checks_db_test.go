package analyzer

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// market-11 owner checks on real PostgreSQL.

func checkAnalyzer(t *testing.T, pool *pgxpool.Pool,
	checks ...config.PreparedCheck) *Analyzer {
	t.Helper()
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.WithCustomChecks(checks)
	a.eval = newCycleEval()
	return a
}

func seedCheckTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		"DROP SCHEMA IF EXISTS market11 CASCADE",
		"CREATE SCHEMA market11",
		"CREATE TABLE market11.a (x int)",
		"CREATE TABLE market11.b (x int)",
		"CREATE TABLE market11.c (x int)",
		"CREATE TABLE market11.d (x int PRIMARY KEY)",
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS market11 CASCADE")
	})
}

const noPKSQL = `SELECT n.nspname || '.' || c.relname AS obj
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'market11' AND c.relkind = 'r'
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
                  WHERE k.conrelid = c.oid AND k.contype = 'p')
ORDER BY 1`

func TestCustomChecks_HappyPathTablesWithoutPK(t *testing.T) {
	pool := phase2Pool(t)
	seedCheckTables(t, pool)
	cleanCategory(t, pool, "custom:no_pk")
	a := checkAnalyzer(t, pool, preparedCheck("no_pk", noPKSQL))
	got := a.runCustomChecks(context.Background())
	if len(got) != 3 {
		t.Fatalf("findings = %d, want 3: %+v", len(got), got)
	}
	want := []string{"market11.a", "market11.b", "market11.c"}
	for i, f := range got {
		if f.ObjectIdentifier != want[i] || f.Category != "custom:no_pk" ||
			f.RecommendedSQL != "" {
			t.Fatalf("finding %d = %+v", i, f)
		}
	}
	if !a.eval.ok["custom:no_pk"] || a.eval.hasFailed("custom:no_pk") {
		t.Fatal("a completed check must mark its category evaluated")
	}
	if err := a.finalizeCycle(context.Background(), got, a.eval.resolvable(got)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.findings
		WHERE category = 'custom:no_pk' AND status = 'open'
		  AND COALESCE(recommended_sql, '') = ''`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("open custom findings without SQL = %d, %v; want 3", n, err)
	}
}

func TestCustomChecks_TruncatesAt101Rows(t *testing.T) {
	pool := phase2Pool(t)
	a := checkAnalyzer(t, pool, preparedCheck("many",
		"SELECT 'obj' || g AS obj FROM generate_series(1, 101) g"))
	got := a.runCustomChecks(context.Background())
	if len(got) != 100 || got[0].Detail["truncated"] != true {
		t.Fatalf("findings = %d (truncated %v), want 100 truncated", len(got),
			len(got) > 0 && got[0].Detail["truncated"] == true)
	}
	if !a.eval.hasFailed("custom:many") {
		t.Fatal("a truncated run cannot prove the unseen rows clear")
	}
}

func TestCustomChecks_RunsReadOnlyAndBounded(t *testing.T) {
	pool := phase2Pool(t)
	sql := `SELECT current_setting('transaction_read_only') AS obj,
		current_setting('statement_timeout') AS st,
		current_setting('lock_timeout') AS lt,
		current_setting('application_name') AS app`
	p := preparedCheck("ro", sql)
	p.TimeoutMs = 1500
	a := checkAnalyzer(t, pool, p)
	got := a.runCustomChecks(context.Background())
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	row, _ := got[0].Detail["row"].(map[string]any)
	if got[0].ObjectIdentifier != "on" || row["st"] != "1500ms" ||
		row["lt"] != "100ms" || row["app"] != "pg_sage" {
		t.Fatalf("session = %q %v, want read-only, 1500ms, 100ms, pg_sage",
			got[0].ObjectIdentifier, row)
	}
}

func TestCustomChecks_WriteFailsInsideTheReadOnlyTransaction(t *testing.T) {
	pool := phase2Pool(t)
	seedCheckTables(t, pool)
	// A statement the parser would refuse; the transaction refuses it too.
	p := preparedCheck("write", "INSERT INTO market11.a VALUES (1) RETURNING x::text AS obj")
	a := checkAnalyzer(t, pool, p)
	if got := a.runCustomChecks(context.Background()); len(got) != 0 {
		t.Fatalf("a write produced findings: %+v", got)
	}
	var n int
	_ = pool.QueryRow(context.Background(), "SELECT count(*) FROM market11.a").Scan(&n)
	if n != 0 || !a.eval.hasFailed("custom:write") {
		t.Fatalf("rows = %d, failed = %v; want 0 and failed", n, a.eval.hasFailed("custom:write"))
	}
}

func TestCustomChecks_TimeoutKeepsOpenFindingsOpen(t *testing.T) {
	pool := phase2Pool(t)
	cleanCategory(t, pool, "custom:slow")
	quick := preparedCheck("slow", "SELECT 'x' AS obj")
	a := checkAnalyzer(t, pool, quick)
	got := a.runCustomChecks(context.Background())
	if err := a.finalizeCycle(context.Background(), got, a.eval.resolvable(got)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	slow := preparedCheck("slow", "SELECT 'x' AS obj FROM pg_sleep(2)")
	slow.TimeoutMs = 100
	b := checkAnalyzer(t, pool, slow)
	got = b.runCustomChecks(context.Background())
	if len(got) != 0 || !b.eval.hasFailed("custom:slow") {
		t.Fatalf("timeout: %d findings, failed %v; want none and failed", len(got),
			b.eval.hasFailed("custom:slow"))
	}
	if err := b.finalizeCycle(context.Background(), got, b.eval.resolvable(got)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if n := countFindingRows(t, pool, "custom:slow", "x", "open"); n != 1 {
		t.Fatalf("open rows after a timeout = %d, want 1", n)
	}
}

func TestCustomChecks_VolatileFunctionRefusedOnFirstRun(t *testing.T) {
	pool := phase2Pool(t)
	var logged []string
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil,
		func(level, msg string, args ...any) { logged = append(logged, msg) })
	a.WithCustomChecks([]config.PreparedCheck{
		preparedCheck("rnd", "SELECT random()::text AS obj", "random"),
		preparedCheck("clock", "SELECT now()::text AS obj", "now"),
	})
	a.eval = newCycleEval()
	got := a.runCustomChecks(context.Background())
	if len(got) != 1 || got[0].Category != "custom:clock" {
		t.Fatalf("findings = %+v, want only custom:clock", got)
	}
	if !a.eval.hasFailed("custom:rnd") {
		t.Fatal("a refused check must keep its open findings")
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "custom check") {
		t.Fatalf("refusal not logged: %q", joined)
	}
}

// Checks for two databases run in parallel, each on its own pool.
func TestCustomChecks_TwoDatabasesInParallel(t *testing.T) {
	poolA := phase2Pool(t)
	poolB := secondDatabasePool(t, poolA)
	sql := "SELECT current_database() AS obj"
	a := checkAnalyzer(t, poolA, preparedCheck("db", sql))
	b := checkAnalyzer(t, poolB, preparedCheck("db", sql))
	var wg sync.WaitGroup
	results := make([][]Finding, 2)
	for i, an := range []*Analyzer{a, b} {
		wg.Add(1)
		go func(i int, an *Analyzer) {
			defer wg.Done()
			results[i] = an.runCustomChecks(context.Background())
		}(i, an)
	}
	wg.Wait()
	if len(results[0]) != 1 || len(results[1]) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0][0].ObjectIdentifier == results[1][0].ObjectIdentifier ||
		results[1][0].ObjectIdentifier != secondDatabaseName {
		t.Fatalf("databases = %q, %q; want two different, the second %s",
			results[0][0].ObjectIdentifier, results[1][0].ObjectIdentifier,
			secondDatabaseName)
	}
}
