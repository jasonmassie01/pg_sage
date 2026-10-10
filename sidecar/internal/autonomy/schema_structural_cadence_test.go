package autonomy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/schemaguard"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// measured.md M9 (v1.8.3): the schema guard's structural scan joins
// pg_attribute with pg_class on every guard cycle (299-314 ms and ~512 MB
// of catalog pages on lifeos, ~17 times an hour) although its answer only
// changes when DDL does. It now runs when the catalog's change counters
// moved (at most every structuralMinInterval) and at least hourly
// (structuralMaxAge); every other cycle reuses its last answer.
//
// The counters are statistics a backend flushes late, so each test runs on
// its own database: setup and DDL sessions are closed (a closing backend
// flushes) and the counters are read until they settle.

// cadenceDatabase is a fresh bootstrapped database and its DSN. Its
// catalog holds no table a pass aggregates, so the tests count the tables
// they create: testdb installs pg_hint_plan where the server offers it
// (CI's does), and the pass leaves out the table the extension owns
// (hint_plan.hints).
func cadenceDatabase(t *testing.T) string {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "sg_cadence")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if n := userTables(t, dsn); n != 0 {
		t.Fatalf("fresh database holds %d user tables, want none", n)
	}
	return dsn
}

// execAndClose runs DDL in a session of its own and closes it, so its
// statistics are flushed.
func execAndClose(t *testing.T, dsn, sql string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("DDL: %v", err)
	}
}

// settledWatermark reads the catalog change counters (each read a fresh
// session) until two reads a second apart agree.
func settledWatermark(t *testing.T, dsn string) int64 {
	t.Helper()
	read := func() int64 {
		conn, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close()
		var n, attributeUpdates int64
		if err := conn.QueryRow(context.Background(), catalogChangeSQL).Scan(&n,
			&attributeUpdates); err != nil {
			t.Fatalf("catalog watermark: %v", err)
		}
		return n
	}
	last := read()
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		now := read()
		if now == last {
			return now
		}
		last = now
	}
	t.Fatal("the catalog change counters never settled")
	return 0
}

func recordingDetector(t *testing.T, dsn string, now func() time.Time) (
	postgresSchemaDetector, *testdb.QueryRecorder) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return newPostgresSchemaDetector(pool, now), rec
}

// structuralRuns counts the structural passes (each lists the tables).
func structuralRuns(rec *testdb.QueryRecorder) int {
	n := len(rec.Matching("structural:tables"))
	rec.Reset()
	return n
}

func detectOnce(t *testing.T, d postgresSchemaDetector) []schemaguard.Invariant {
	t.Helper()
	items, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	return items
}

func TestStructuralScan_RunsOnlyAfterDDLOrHourly(t *testing.T) {
	dsn := cadenceDatabase(t)
	settledWatermark(t, dsn)
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	detector, rec := recordingDetector(t, dsn, func() time.Time { return clock })
	detectOnce(t, detector)
	if n := structuralRuns(rec); n != 1 {
		t.Fatalf("first cycle ran the structural scan %d times, want 1", n)
	}
	clock = clock.Add(10 * time.Minute)
	detectOnce(t, detector)
	if n := structuralRuns(rec); n != 0 {
		t.Fatalf("a cycle without DDL ran the structural scan %d times, want 0", n)
	}
	schemaName := fmt.Sprintf("sg_cadence_%d", time.Now().UnixNano())
	ddl(t, dsn, fmt.Sprintf(`CREATE SCHEMA %[1]s;
		CREATE TABLE %[1]s.orders (account_id text, n int)`, schemaName))
	clock = clock.Add(time.Minute)
	found := false
	for _, item := range detectOnce(t, detector) {
		found = found || (item.Schema == schemaName &&
			item.Kind == schemaguard.InvariantTypeTightening)
	}
	if n := structuralRuns(rec); n != 1 || !found {
		t.Fatalf("after DDL: %d structural scans (want 1), new invariant found %v", n, found)
	}
	ddl(t, dsn, fmt.Sprintf(`CREATE TABLE %s.refunds (order_id text, n int)`, schemaName))
	clock = clock.Add(time.Minute) // inside the minimum interval since the scan
	detectOnce(t, detector)
	if n := structuralRuns(rec); n != 0 {
		t.Fatalf("a cycle inside the minimum interval ran the scan %d times, want 0", n)
	}
	clock = clock.Add(structuralMinInterval)
	detectOnce(t, detector)
	if n := structuralRuns(rec); n != 1 {
		t.Fatalf("after the minimum interval the changed catalog was scanned %d times, "+
			"want 1", n)
	}
	clock = clock.Add(structuralMaxAge)
	detectOnce(t, detector)
	if n := structuralRuns(rec); n != 1 {
		t.Fatalf("an hour later the structural scan ran %d times, want 1 (the floor)", n)
	}
}

// ddl runs DDL in a session of its own and waits until the catalog change
// counters show it.
func ddl(t *testing.T, dsn, sql string) {
	t.Helper()
	before := settledWatermark(t, dsn)
	execAndClose(t, dsn, sql)
	if settledWatermark(t, dsn) == before {
		t.Fatal("DDL did not move the catalog change counters")
	}
}

// Between scans the reused answer is the last one, unchanged; a detector
// without a cache (the zero value) scans every time.
func TestStructuralScan_ReusedAnswerIsTheLastOne(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, `CREATE TABLE public.ledger (customer_id text, n int)`)
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	detector, rec := recordingDetector(t, dsn, func() time.Time { return clock })
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
	if structuralRuns(rec) != 1 || len(first) == 0 || len(first) != len(second) {
		t.Fatalf("reused answer: %d vs %d items", len(first), len(second))
	}
	for i := range first {
		if first[i].Target() != second[i].Target() || first[i].Subject != second[i].Subject {
			t.Fatalf("item %d: %s/%s vs %s/%s", i, first[i].Target(), first[i].Subject,
				second[i].Target(), second[i].Subject)
		}
	}
	second[0].Subject = "mutated"
	third, err := detector.detectStructuralPathologies(ctx)
	if err != nil || third[0].Subject == "mutated" {
		t.Fatalf("a caller's change leaked into the cached answer (%v)", err)
	}
	uncached := postgresSchemaDetector{pool: detector.pool}
	for i := 0; i < 2; i++ {
		if _, err := uncached.detectStructuralPathologies(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := structuralRuns(rec); n != 2 {
		t.Fatalf("an uncached detector scanned %d times in 2 calls, want 2", n)
	}
}
