package explain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// connState is what a pooled connection must look like after EXPLAIN:
// no open transaction, the server's statement_timeout, and no pg_sage
// prepared statement (_sage_explain) left behind. pgx's own statement
// cache (stmtcache_*) is not pg_sage's and is not counted.
func connState(t *testing.T, pool *pgxpool.Pool) (readOnly, timeout string, prepared int) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT
		pg_catalog.current_setting('transaction_read_only'),
		pg_catalog.current_setting('statement_timeout'),
		(SELECT count(*) FROM pg_catalog.pg_prepared_statements
			WHERE name LIKE '%sage%')`).Scan(
		&readOnly, &timeout, &prepared)
	if err != nil {
		t.Fatalf("read connection state: %v", err)
	}
	return readOnly, timeout, prepared
}

// A guard-approved query that outlives the timeout is cancelled, and the
// one pooled connection comes back clean; a fast query still analyzes.
func checkGuardTimeoutBoundary(t *testing.T, inspect inspectFn) {
	base := liveExplainPool(t)
	cfg := base.Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("single-connection pool: %v", err)
	}
	t.Cleanup(pool.Close)
	_, wantTimeout, _ := connState(t, pool)
	ex := guardExplainer(pool, 300, inspect)
	const fast = "SELECT count(*) FROM generate_series(1, 10) g"
	assertAnalyzed(t, ex, fast)
	_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_results")
	start := time.Now()
	_, err = ex.Explain(context.Background(),
		ExplainRequest{Query: "SELECT count(*) FROM generate_series(1, 2000000000) g"})
	if err == nil {
		t.Fatal("a query past the statement timeout was not cancelled")
	}
	if !strings.Contains(err.Error(), "statement timeout") &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancel error = %v, want a statement timeout or deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %v with a 300ms timeout", elapsed)
	}
	readOnly, timeout, prepared := connState(t, pool)
	if readOnly != "off" || timeout != wantTimeout || prepared != 0 {
		t.Errorf("connection after timeout: read_only=%s statement_timeout=%s "+
			"(want %s) prepared=%d", readOnly, timeout, wantTimeout, prepared)
	}
	assertAnalyzed(t, ex, fast)
}

// Concurrent guard calls on one Explainer share no decision state: each
// safe query is analyzed and each volatile one refused.
func checkGuardConcurrent(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	execFixture(t, pool, guardFixtureSQL, guardCleanupSQL)
	_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_results")
	ex := guardExplainer(pool, 10000, inspect)
	var wg sync.WaitGroup
	for i := 0; i < concurrentQueries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			query := concurrentQuery(i)
			res, err := ex.Explain(context.Background(), ExplainRequest{Query: query})
			switch {
			case err != nil:
				t.Errorf("%q: %v", query, err)
			case i%2 == 0 && (res.ActualTimeMs == nil || res.AnalyzeRefused != ""):
				t.Errorf("%q not analyzed: refused=%q", query, res.AnalyzeRefused)
			case i%2 == 1 && (res.ActualTimeMs != nil ||
				!strings.Contains(res.AnalyzeRefused, "volatile function eg_volatile")):
				t.Errorf("%q not refused: refused=%q", query, res.AnalyzeRefused)
			}
		}(i)
	}
	wg.Wait()
	assertSideEffectsAbsent(t, pool)
}

func TestGuardCatalogAnalyzesPlainSelect(t *testing.T) {
	checkGuardAnalyzesPlainSelect(t, recordedInspector())
}

func TestGuardCatalogRefusesSideEffects(t *testing.T) {
	checkGuardRefusesSideEffects(t, recordedInspector())
}

func TestGuardCatalogRefusesImplicitCast(t *testing.T) {
	checkGuardRefusesImplicitCast(t, recordedInspector())
}

func TestGuardCatalogUnknownObjects(t *testing.T) {
	checkGuardUnknownObjects(t, recordedInspector())
}

func TestGuardCatalogViewDepth(t *testing.T) {
	checkGuardViewDepth(t, recordedInspector())
}

func TestGuardCatalogRereadsVolatility(t *testing.T) {
	checkGuardRereadsVolatility(t, recordedInspector())
}

func TestGuardCatalogTimeoutBoundary(t *testing.T) {
	checkGuardTimeoutBoundary(t, recordedInspector())
}

func TestGuardCatalogConcurrent(t *testing.T) {
	checkGuardConcurrent(t, recordedInspector())
}
