package collector

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: one collector cycle on one pool; the pin's
// concurrency is selfmonitor's.

// sagePoolOn opens a pool on dsn configured exactly as pg_sage's own.
func sagePoolOn(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// security-01c-r: with public.lower(text) and public.= operators planted
// ahead of pg_catalog on the database's path, one collector cycle on
// pg_sage's pool calls none of them and nothing comes to depend on them.
func TestCollectorCycleCallsNoPlantedObject(t *testing.T) {
	plant := testdb.PlantedPathDatabase(t, "plant_coll")
	pool := sagePoolOn(t, plant.DSN)
	ctx := context.Background()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	snap, err := New(pool, testConfig(), version, noopLog).collect(ctx)
	if err != nil {
		t.Fatalf("collector cycle: %v", err)
	}
	if snap == nil || len(snap.Tables) == 0 && len(snap.Queries) == 0 &&
		snap.System.TotalBackends == 0 {
		t.Fatalf("collector cycle produced no data: %+v", snap)
	}
	for category, reason := range snap.Unavailable {
		t.Errorf("category %s unavailable on the pinned pool: %s", category, reason)
	}
	plant.AssertNoPlantedCalls(t, pool)
}
