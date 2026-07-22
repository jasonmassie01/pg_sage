package autoexplain

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

func TestCaptureOnDemand_NormalizedParameterizedQuery(t *testing.T) {
	pool := requireAuditFixturePool(t)
	ctx := context.Background()
	bootstrapCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := schema.Bootstrap(bootstrapCtx, pool); err != nil {
		t.Fatalf("bootstrap designated fixture schema: %v", err)
	}
	queryID := int64(999999908)
	query := "SELECT $1::integer"

	cleanup := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.explain_cache WHERE queryid = $1", queryID)
	}
	cleanup()
	t.Cleanup(cleanup)

	collector := NewCollector(
		pool,
		CollectorConfig{
			CollectIntervalSeconds: 60,
			MaxPlansPerCycle:       5,
			LogMinDurationMs:       100,
		},
		&Availability{Available: false, Method: "unavailable"},
		func(string, string, ...any) {},
	)

	if err := collector.captureOnDemand(ctx, queryID, query); err != nil {
		t.Fatalf("capture normalized parameterized query: %v", err)
	}

	var storedQuery string
	var planJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT query_text, plan_json
		  FROM sage.explain_cache
		 WHERE queryid = $1
		 ORDER BY captured_at DESC
		 LIMIT 1`, queryID).Scan(&storedQuery, &planJSON); err != nil {
		t.Fatalf("read captured parameterized plan: %v", err)
	}
	if storedQuery != query {
		t.Fatalf("stored query = %q, want %q", storedQuery, query)
	}
	if len(planJSON) == 0 {
		t.Fatal("captured parameterized plan is empty")
	}
}


func requireAuditFixturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SAGE_TEST_DATABASE_URL"))
	if dsn == "" || strings.Contains(dsn, "pgsage_test_disabled") {
		t.Skip("SKIPPED: SAGE_TEST_DATABASE_URL fixture is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create designated fixture pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping designated fixture: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
