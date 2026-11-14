package ask

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: one tool call on one pool; the pin's
// concurrency is selfmonitor's.

// security-01c-r: with public.lower(text) and public.= operators planted
// ahead of pg_catalog on the database's path, an Ask Sage describe_table
// tool call on pg_sage's pool calls none of them.
func TestAskToolCallCallsNoPlantedObject(t *testing.T) {
	plant := testdb.PlantedPathDatabase(t, "plant_ask")
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
	for _, stmt := range []string{
		"CREATE TABLE public.plant_orders (id int PRIMARY KEY, status text NOT NULL)",
		"CREATE INDEX plant_orders_status ON public.plant_orders (status)",
		"COMMENT ON TABLE public.plant_orders IS 'Planted-path orders'",
	} {
		if _, err := plant.Admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	s, err := New(Deps{Database: "testdb", Pool: pool, Config: testAskConfig(),
		Settings: config.DefaultConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := mustTool(t, s, viewer, "describe_table", `{"table":"public.plant_orders"}`)
	for _, want := range []string{"Planted-path orders", "status", "plant_orders_status"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("describe_table text lacks %q:\n%s", want, out.Text)
		}
	}
	plant.AssertNoPlantedCalls(t, pool)
}
