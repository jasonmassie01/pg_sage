package optimizer_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/schema"
)

func TestPlanCaptureObservedAutoExplainWithoutNativeExtension(t *testing.T) {
	pool := observedPlanTestPool(t)
	defer pool.Close()
	if err := schema.Bootstrap(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	observed := autoexplain.ObservedPlan{QueryID: 87771, Query: "SELECT 1", TotalCost: 12,
		CapturedAt: time.Now().Add(-time.Minute),
		JSON:       []byte(`[{"Plan":{"Node Type":"Index Scan","Total Cost":12}}]`)}
	if err := autoexplain.StoreObservedPlan(t.Context(), pool, observed); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(t.Context(), `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, total_cost, execution_time, captured_at)
		VALUES ($1, 'SELECT 1', '[{"Plan":{"Node Type":"Seq Scan","Total Cost":99}}]',
		'auto_explain', 99, 1, now())`, observed.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	planner := optimizer.NewPlanCapture(pool, 170000, false, true, "auto_explain",
		func(string, string, ...any) {})
	queries := []collector.QueryStats{{QueryID: observed.QueryID, Query: observed.Query}}
	plans, source := planner.CapturePlans(t.Context(), queries)
	if source != "auto_explain" || len(plans) != 1 || plans[0].ScanType != "Index Scan" {
		t.Fatalf("real observed execution was not preferred: %s %+v", source, plans)
	}
	_, err = pool.Exec(t.Context(),
		"DELETE FROM sage.explain_cache WHERE queryid=$1 AND source='auto_explain_log'", observed.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	plans, source = planner.CapturePlans(t.Context(), queries)
	if source != "auto_explain" || len(plans) != 1 || plans[0].ScanType != "Seq Scan" {
		t.Fatalf("legacy auto_explain fallback disappeared: %s %+v", source, plans)
	}
}

func observedPlanTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid fixture configuration")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skip("PostgreSQL fixture absent; observed-plan consumption not verified")
	}
	return pool
}
