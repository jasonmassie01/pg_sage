package selfconfig

import (
	"strings"
	"testing"
)

// footprint-05: the catalog probe must time the statistics it claims to
// read. A count(*) over a LEFT JOIN to pg_stat_all_tables that references
// no view column lets the planner replace every pg_stat_get_* output with
// NULL (measured on PG17: the view's HashAggregate outputs NULL::bigint),
// so the probe timed a plain pg_class scan. EXPLAIN VERBOSE pins that the
// probe's plan calls the statistics functions (run on PG14 and PG17).

func TestCatalogProbeCallsTheStatisticsFunctions(t *testing.T) {
	pool, ctx := testPool(t)
	rows, err := pool.Query(ctx, "EXPLAIN (VERBOSE, COSTS OFF) "+CatalogScanSQL)
	if err != nil {
		t.Fatalf("explain the catalog probe: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("plan line: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan: %v", err)
	}
	var version string
	_ = pool.QueryRow(ctx, "SHOW server_version_num").Scan(&version)
	if !strings.Contains(plan.String(), "pg_stat_get_") {
		t.Fatalf("server %s: the catalog probe's plan calls no pg_stat_get_* function, so "+
			"it does not time the statistics read:\n%s", version, plan.String())
	}
}

// The probe still counts relations: one row per pg_class entry.
func TestCatalogProbeStillCountsRelations(t *testing.T) {
	pool, ctx := testPool(t)
	var want float64
	if err := pool.QueryRow(ctx, "SELECT count(*)::float8 FROM pg_catalog.pg_class").
		Scan(&want); err != nil {
		t.Fatal(err)
	}
	ev, err := Gather(ctx, pool)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if !ev.Relations.Known || ev.Relations.Value != want {
		t.Fatalf("relations %+v, want %v", ev.Relations, want)
	}
	if ev.Source != SourceProbe {
		t.Fatalf("Gather source %q, want %q", ev.Source, SourceProbe)
	}
}
