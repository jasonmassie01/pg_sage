package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: bootstrap's own advisory lock is covered by
// bootstrap_lock_test.go.

// security-01c-r: production bootstraps through a pool built with
// ConfigurePool, so its migrations run on search_path = pg_catalog, pg_temp.
// They run twice cleanly there on a fresh database whose own path puts
// planted public objects first, and call none of them.
func TestBootstrapTwiceOnThePinnedPool(t *testing.T) {
	plant := testdb.PlantedPathDatabase(t, "plant_boot")
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
	for run := 1; run <= 2; run++ {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d on the pinned pool: %v", run, err)
		}
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables
		WHERE schemaname = 'sage'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables < 20 {
		t.Fatalf("sage schema has %d tables after bootstrap, want the full schema", tables)
	}
	plant.AssertNoPlantedCalls(t, pool)
}
