//go:build integration

package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// ---------- test helpers ----------

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.SkipUnlessLive(t)
	testPoolOnce.Do(func() {
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		if err := schema.Bootstrap(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
	})
	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, ctx
}

func testExplainConfig() *config.ExplainConfig {
	return &config.ExplainConfig{
		Enabled:         true,
		TimeoutMs:       5000,
		CacheTTLMinutes: 60,
	}
}

func noopLog(_ string, _ string, _ ...any) {}

// cleanExplainResults removes all cached explain results before a test.
func cleanExplainResults(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
) {
	t.Helper()
	_, err := pool.Exec(ctx, "DELETE FROM sage.explain_results")
	if err != nil {
		t.Fatalf("clean explain_results: %v", err)
	}
}

// ---------- tests ----------

func TestExplain_SimpleSelect(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)

	ex := New(pool, testExplainConfig(), noopLog)
	result, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT 1",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if result.Query != "SELECT 1" {
		t.Errorf("Query = %q, want %q", result.Query, "SELECT 1")
	}
	if !json.Valid(result.PlanJSON) {
		t.Errorf("PlanJSON is not valid JSON: %s", result.PlanJSON)
	}
	if result.EstimatedCost <= 0 {
		t.Errorf("EstimatedCost = %f, want > 0", result.EstimatedCost)
	}
	if len(result.NodeBreakdown) < 1 {
		t.Error("NodeBreakdown is empty, want at least 1 node")
	}
}

func TestExplain_AnalyzeMode(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)

	ex := New(pool, testExplainConfig(), noopLog)
	result, err := ex.Explain(ctx, ExplainRequest{
		Query:    "SELECT generate_series(1,100)",
		PlanOnly: false,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if result.ActualTimeMs == nil {
		t.Error("ActualTimeMs is nil, expected non-nil (ANALYZE was used)")
	}
}

func TestExplain_PlanOnlyMode(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)

	ex := New(pool, testExplainConfig(), noopLog)
	result, err := ex.Explain(ctx, ExplainRequest{
		Query:    "SELECT 1",
		PlanOnly: true,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if result.ActualTimeMs != nil {
		t.Errorf(
			"ActualTimeMs = %f, want nil for PlanOnly mode",
			*result.ActualTimeMs,
		)
	}
	if !strings.Contains(result.Note, "without ANALYZE") {
		t.Errorf(
			"Note = %q, want it to contain %q",
			result.Note, "without ANALYZE",
		)
	}
}

func TestExplain_CacheRoundTrip(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)

	ex := New(pool, testExplainConfig(), noopLog)
	query := "SELECT 1"

	// First call: should not be cached.
	r1, err := ex.Explain(ctx, ExplainRequest{Query: query})
	if err != nil {
		t.Fatalf("Explain (first): %v", err)
	}
	if r1.CachedAt != nil {
		t.Errorf(
			"first call CachedAt = %v, want nil (not from cache)",
			*r1.CachedAt,
		)
	}

	// Second call: should hit the cache.
	r2, err := ex.Explain(ctx, ExplainRequest{Query: query})
	if err != nil {
		t.Fatalf("Explain (second): %v", err)
	}
	if r2.CachedAt == nil {
		t.Error("second call CachedAt is nil, expected non-nil (cache hit)")
	}
}

func TestExplain_CacheExpiry(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)

	// Insert an expired cache entry manually, under the key an ANALYZE
	// request for this body uses.
	query := "SELECT 1"
	hash := cacheKey(query, nil, false)
	dbName := "" // pool connects to the same DB; databaseName() returns the DB name

	// Determine what databaseName() will return for this pool.
	ex := New(pool, testExplainConfig(), noopLog)
	dbName = ex.databaseName()

	_, err := pool.Exec(ctx,
		`INSERT INTO sage.explain_results
			(query_hash, expires_at, plan_json, explanation, database_name)
		 VALUES ($1, now() - interval '1 hour', '[]'::jsonb, '{}'::jsonb, $2)
		 ON CONFLICT (query_hash, database_name) DO UPDATE
		 SET expires_at = now() - interval '1 hour'`,
		hash, dbName,
	)
	if err != nil {
		t.Fatalf("insert expired cache entry: %v", err)
	}

	// Explain should NOT use the expired cache entry.
	result, err := ex.Explain(ctx, ExplainRequest{Query: query})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if result.CachedAt != nil {
		t.Errorf(
			"CachedAt = %v, want nil (expired entry should not be used)",
			*result.CachedAt,
		)
	}
}

func TestExplain_DDLRejected(t *testing.T) {
	pool, ctx := requireDB(t)

	ex := New(pool, testExplainConfig(), noopLog)
	_, err := ex.Explain(ctx, ExplainRequest{
		Query: "CREATE TABLE test_bogus (id int)",
	})
	if err == nil {
		t.Fatal("expected error for DDL query, got nil")
	}
	if !strings.Contains(err.Error(), "DDL") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "DDL")
	}
}

func TestExplain_EmptyQuery(t *testing.T) {
	pool, ctx := requireDB(t)

	ex := New(pool, testExplainConfig(), noopLog)
	_, err := ex.Explain(ctx, ExplainRequest{
		Query:   "",
		QueryID: 0,
	})
	if err == nil {
		t.Fatal("expected error for empty query and zero query_id, got nil")
	}
}

func TestExplain_ParamPlaceholder(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)

	ex := New(pool, testExplainConfig(), noopLog)
	// The Explainer handles $N placeholders via runExplainParameterized:
	// PREPARE ... AS <query>; EXPLAIN (FORMAT JSON) EXECUTE ...(NULL, ...).
	// Confirm the path succeeds and returns a usable plan rather than an error.
	result, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT $1::int",
	})
	if err != nil {
		t.Fatalf("parameterized query: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result for parameterized query")
	}
	if len(result.PlanJSON) == 0 {
		t.Error("expected non-empty PlanJSON for parameterized query")
	}
	if result.EstimatedCost < 0 {
		t.Errorf("EstimatedCost = %.2f, want non-negative", result.EstimatedCost)
	}
}

func TestExplain_ComplexQuery(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)

	ex := New(pool, testExplainConfig(), noopLog)
	result, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT * FROM pg_class c " +
			"JOIN pg_namespace n ON c.relnamespace = n.oid " +
			"LIMIT 10",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if len(result.NodeBreakdown) < 2 {
		t.Errorf(
			"NodeBreakdown has %d nodes, want at least 2 for a join query",
			len(result.NodeBreakdown),
		)
	}
}

func TestExplain_InvalidSQL(t *testing.T) {
	pool, ctx := requireDB(t)

	ex := New(pool, testExplainConfig(), noopLog)
	_, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT * FROM nonexistent_table_xyz_99",
	})
	if err == nil {
		t.Fatal("expected error for invalid SQL, got nil")
	}
}

// tuning-10: an id with no statement in this database is a client error
// (HTTP 400), whether or not pg_stat_statements is installed.
func TestExplain_UnknownQueryIDIsInvalid_LivePool(t *testing.T) {
	pool, ctx := requireDB(t)

	ex := New(pool, testExplainConfig(), noopLog)
	_, err := ex.Explain(ctx, ExplainRequest{QueryID: 12345})
	if !errors.Is(err, ErrExplainInvalidRequest) {
		t.Fatalf("err = %v, want ErrExplainInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("err = %v, the lookup is implemented now", err)
	}
}

// seedCacheKeyTable creates an analyzed table whose point lookup uses the
// primary key and whose range scan reads every row.
func seedCacheKeyTable(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	stmts := []string{
		"DROP TABLE IF EXISTS tuning07_cache",
		"CREATE TABLE tuning07_cache (id int PRIMARY KEY, a text NOT NULL)",
		"INSERT INTO tuning07_cache SELECT g, 'v' || g FROM generate_series(1, 20000) g",
		"ANALYZE tuning07_cache",
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS tuning07_cache")
	})
}

func explainOrFatal(
	t *testing.T, ex *Explainer, ctx context.Context, q string, params ...string,
) *ExplainResult {
	t.Helper()
	r, err := ex.Explain(ctx, ExplainRequest{Query: q, Params: params})
	if err != nil {
		t.Fatalf("Explain(%q): %v", q, err)
	}
	if len(r.NodeBreakdown) == 0 {
		t.Fatalf("Explain(%q) returned no plan nodes", q)
	}
	return r
}

// tuning-07: a '--' inside a literal must not hide the rest of the
// statement from the cache key, so a different predicate gets its own plan.
func TestExplain_CacheKeyKeepsLiteralCommentMarkers(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)
	seedCacheKeyTable(t, pool, ctx)
	ex := New(pool, testExplainConfig(), noopLog)

	point := "SELECT * FROM tuning07_cache WHERE a = '--x' AND id = 1"
	scan := "SELECT * FROM tuning07_cache WHERE a = '--x' AND id < 100000"
	first := explainOrFatal(t, ex, ctx, point)
	if first.CachedAt != nil {
		t.Fatalf("first point lookup served from cache at %v", *first.CachedAt)
	}
	second := explainOrFatal(t, ex, ctx, scan)
	if second.CachedAt != nil {
		t.Fatalf("range scan served the point lookup's cached plan (cached at %v)",
			*second.CachedAt)
	}
	if string(first.PlanJSON) == string(second.PlanJSON) {
		t.Fatalf("both statements returned the same PlanJSON: %s", first.PlanJSON)
	}
	if !strings.Contains(first.NodeBreakdown[0].NodeType, "Index") ||
		second.NodeBreakdown[0].NodeType != "Seq Scan" {
		t.Errorf("root nodes = %q and %q, want an index scan then Seq Scan",
			first.NodeBreakdown[0].NodeType, second.NodeBreakdown[0].NodeType)
	}
	repeat := explainOrFatal(t, ex, ctx, point)
	if repeat.CachedAt == nil {
		t.Error("repeat of the point lookup missed the cache")
	}
	if repeat.NodeBreakdown[0].NodeType != first.NodeBreakdown[0].NodeType {
		t.Errorf("cached repeat root = %q, want %q",
			repeat.NodeBreakdown[0].NodeType, first.NodeBreakdown[0].NodeType)
	}
	hinted := explainOrFatal(t, ex, ctx, "/*+ SeqScan(tuning07_cache) */ "+point)
	if hinted.CachedAt != nil {
		t.Error("hinted statement served the unhinted statement's cached plan")
	}
}

// tuning-07: "$N" inside a literal or dollar quote is not a bind parameter,
// so the statement takes the plain EXPLAIN path, where ANALYZE is either
// run or refused by the analyze guard (always refused in a build without
// cgo). The PREPARE path never considers ANALYZE.
func TestExplain_LiteralDollarIsNotAParameter(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)
	ex := New(pool, testExplainConfig(), noopLog)

	for _, q := range []string{"SELECT '$5'::text AS v", "SELECT $q$ $1 $q$ AS v"} {
		r := explainOrFatal(t, ex, ctx, q)
		analyzeConsidered := r.ActualTimeMs != nil || r.AnalyzeRefused != ""
		if strings.Contains(r.Note, "query has parameters") || !analyzeConsidered {
			t.Errorf("%q took the parameterized path: note=%q refused=%q",
				q, r.Note, r.AnalyzeRefused)
		}
	}
	mixed := explainOrFatal(t, ex, ctx, "SELECT $1::int AS a, '$2' AS b", "7")
	if !strings.Contains(mixed.Note, "query has parameters") ||
		mixed.ActualTimeMs != nil || mixed.AnalyzeRefused != "" {
		t.Errorf("one-parameter query: note=%q refused=%q, want the parameter path",
			mixed.Note, mixed.AnalyzeRefused)
	}
}
