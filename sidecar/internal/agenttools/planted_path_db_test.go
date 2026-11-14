package agenttools

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: one tool call on one pool; the pin's
// concurrency is selfmonitor's.

// security-01c-r: with public.lower(text) and public.= operators planted
// ahead of pg_catalog on the database's path, the MCP top_queries read
// tool (with plans) on pg_sage's pool calls none of them.
func TestTopQueriesCallsNoPlantedObject(t *testing.T) {
	plant := testdb.PlantedPathDatabase(t, "plant_mcp")
	cfg, err := pgxpool.ParseConfig(plant.DSN)
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap on the pinned pool: %v", err)
	}
	const workload = "SELECT count(*) FROM public.plant_orders WHERE status = $1"
	for _, stmt := range []string{
		"CREATE TABLE public.plant_orders (id int PRIMARY KEY, status text NOT NULL)",
		"INSERT INTO public.plant_orders SELECT i, 'open' FROM generate_series(1, 50) i",
	} {
		if _, err := plant.Admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := plant.Admin.Exec(ctx, workload, "open"); err != nil {
			t.Fatalf("workload: %v", err)
		}
	}
	res, err := New(pool, Options{}).TopQueries(ctx,
		TopQueriesRequest{Limit: 50, IncludePlans: true})
	if err != nil {
		t.Fatalf("top_queries on the pinned pool: %v", err)
	}
	found := false
	for _, q := range res.Queries {
		found = found || strings.Contains(q.Query, "plant_orders")
	}
	if !found {
		t.Fatalf("top_queries lacks the workload statement among %d", len(res.Queries))
	}
	plant.AssertNoPlantedCalls(t, pool)
}
