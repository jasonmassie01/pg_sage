package autonomy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schemaguard"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// measured.md M9 (v1.8.3): the schema guard's structural scan joins
// pg_attribute with pg_class on every guard cycle (299-314 ms and ~512 MB
// of catalog pages on lifeos, ~17 times an hour) although its answer only
// changes when DDL does. It now runs when the catalog's change counters
// moved (at most every structuralMinInterval) and at least hourly
// (structuralMaxAge), and every other cycle reuses its last answer.

func recordingDetector(t *testing.T, now func() time.Time) (postgresSchemaDetector,
	*testdb.QueryRecorder) {
	t.Helper()
	base := requireAutonomyDB(t)
	cfg := base.Config().Copy()
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return newPostgresSchemaDetector(pool, now), rec
}

func structuralRuns(rec *testdb.QueryRecorder) int {
	n := len(rec.Matching("everything_text", "type_tightening"))
	rec.Reset()
	return n
}

// catalogWatermark is the detector's change signal, read until DDL made
// by another session is visible in it (statistics are flushed late).
func waitForCatalogChange(t *testing.T, pool *pgxpool.Pool, before int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = pool.Exec(context.Background(), "SELECT pg_stat_force_next_flush()")
		var now int64
		if err := pool.QueryRow(context.Background(), catalogChangeSQL).Scan(&now); err != nil {
			t.Fatalf("catalog watermark: %v", err)
		}
		if now != before {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the catalog change counters never moved after DDL")
}

func TestStructuralScan_RunsOnlyAfterDDLOrHourly(t *testing.T) {
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	detector, rec := recordingDetector(t, func() time.Time { return clock })
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	if _, err := detector.Detect(ctx); err != nil {
		t.Fatalf("first detect: %v", err)
	}
	if n := structuralRuns(rec); n != 1 {
		t.Fatalf("first cycle ran the structural scan %d times, want 1", n)
	}
	clock = clock.Add(10 * time.Minute)
	if _, err := detector.Detect(ctx); err != nil {
		t.Fatalf("second detect: %v", err)
	}
	if n := structuralRuns(rec); n != 0 {
		t.Fatalf("a cycle without DDL ran the structural scan %d times, want 0", n)
	}
	var before int64
	if err := pool.QueryRow(ctx, catalogChangeSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	schemaName := fmt.Sprintf("sg_cadence_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %[1]s;
		CREATE TABLE %[1]s.orders (account_id text, n int)`, schemaName)); err != nil {
		t.Fatalf("DDL: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	})
	waitForCatalogChange(t, pool, before)
	clock = clock.Add(structuralMinInterval)
	items, err := detector.Detect(ctx)
	if err != nil {
		t.Fatalf("detect after DDL: %v", err)
	}
	if n := structuralRuns(rec); n != 1 {
		t.Fatalf("the cycle after DDL ran the structural scan %d times, want 1", n)
	}
	found := false
	for _, item := range items {
		found = found || (item.Schema == schemaName &&
			item.Kind == schemaguard.InvariantTypeTightening)
	}
	if !found {
		t.Fatal("the new table's text id column was not detected after DDL")
	}
	clock = clock.Add(structuralMaxAge)
	if _, err := detector.Detect(ctx); err != nil {
		t.Fatalf("hourly detect: %v", err)
	}
	if n := structuralRuns(rec); n != 1 {
		t.Fatalf("an hour later the structural scan ran %d times, want 1 (the floor)", n)
	}
}

// Between scans the reused answer is the last one, unchanged.
func TestStructuralScan_ReusedAnswerIsTheLastOne(t *testing.T) {
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	detector, rec := recordingDetector(t, func() time.Time { return clock })
	ctx := context.Background()
	first, err := detector.detectStructuralPathologies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	second, err := detector.detectStructuralPathologies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if structuralRuns(rec) != 1 || len(first) != len(second) {
		t.Fatalf("reused answer differs: %d vs %d items", len(first), len(second))
	}
	for i := range first {
		if first[i].Target() != second[i].Target() || first[i].Subject != second[i].Subject {
			t.Fatalf("item %d: %s/%s vs %s/%s", i, first[i].Target(), first[i].Subject,
				second[i].Target(), second[i].Subject)
		}
	}
}
