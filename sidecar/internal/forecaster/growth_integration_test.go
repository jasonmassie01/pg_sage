//go:build integration

package forecaster

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres" +
		"?sslmode=disable"
}

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	testPoolOnce.Do(func() {
		dsn := testDSN()
		qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}

		testPool, testPoolErr = pgxpool.NewWithConfig(qctx, poolCfg)
		if testPoolErr != nil {
			return
		}

		if err := testPool.Ping(qctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}

		if err := schema.Bootstrap(qctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		schema.ReleaseAdvisoryLock(qctx, testPool)
	})

	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, ctx
}

// cleanupObject removes rows for a specific object_name so tests
// do not interfere with each other.
func cleanupObject(
	t *testing.T, pool *pgxpool.Pool, objectName string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()
	_, err := pool.Exec(ctx,
		"DELETE FROM sage.size_history WHERE object_name = $1",
		objectName,
	)
	if err != nil {
		t.Fatalf("cleanup %s: %v", objectName, err)
	}
}

// --- Test 1 ---

func TestRecordSizeHistory_Insert(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_insert_basic"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	err := RecordSizeHistory(
		ctx, pool, "table", obj, 1000, nil, "testdb",
	)
	if err != nil {
		t.Fatalf("RecordSizeHistory: %v", err)
	}

	// Read back and verify all fields.
	var (
		id           int64
		collectedAt  time.Time
		metricType   string
		objectName   string
		sizeBytes    int64
		deadTuplePct *float64
		dbName       *string
	)
	err = pool.QueryRow(ctx,
		`SELECT id, collected_at, metric_type, object_name,
		        size_bytes, dead_tuple_pct, database_name
		 FROM sage.size_history
		 WHERE object_name = $1`, obj,
	).Scan(
		&id, &collectedAt, &metricType, &objectName,
		&sizeBytes, &deadTuplePct, &dbName,
	)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if id < 1 {
		t.Errorf("id: got %d, want positive", id)
	}
	if collectedAt.IsZero() {
		t.Error("collected_at is zero")
	}
	if metricType != "table" {
		t.Errorf("metric_type: got %q, want %q", metricType, "table")
	}
	if objectName != obj {
		t.Errorf("object_name: got %q, want %q", objectName, obj)
	}
	if sizeBytes != 1000 {
		t.Errorf("size_bytes: got %d, want 1000", sizeBytes)
	}
	if deadTuplePct != nil {
		t.Errorf(
			"dead_tuple_pct: got %v, want nil", *deadTuplePct,
		)
	}
	if dbName == nil || *dbName != "testdb" {
		t.Errorf("database_name: got %v, want 'testdb'", dbName)
	}
}

// --- Test 2 ---

func TestRecordSizeHistory_WithDeadTuples(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_insert_dead_tuples"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	pct := 5.25
	err := RecordSizeHistory(
		ctx, pool, "table", obj, 2000, &pct, "testdb",
	)
	if err != nil {
		t.Fatalf("RecordSizeHistory: %v", err)
	}

	var gotPct *float64
	err = pool.QueryRow(ctx,
		`SELECT dead_tuple_pct FROM sage.size_history
		 WHERE object_name = $1`, obj,
	).Scan(&gotPct)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotPct == nil {
		t.Fatal("dead_tuple_pct: got nil, want 5.25")
	}
	if *gotPct != 5.25 {
		t.Errorf(
			"dead_tuple_pct: got %.2f, want 5.25", *gotPct,
		)
	}
}

// --- Test 3 ---

func TestRecordSizeHistory_AllMetricTypes(t *testing.T) {
	pool, ctx := requireDB(t)

	types := []string{"database", "table", "wal_slot"}
	for _, mt := range types {
		obj := fmt.Sprintf("test_metric_type_%s", mt)
		t.Cleanup(func() { cleanupObject(t, pool, obj) })

		err := RecordSizeHistory(
			ctx, pool, mt, obj, 500, nil, "testdb",
		)
		if err != nil {
			t.Errorf(
				"RecordSizeHistory(%s): %v", mt, err,
			)
		}
	}
}

// --- Test 4 ---

func TestRecordSizeHistory_InvalidMetricType(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_invalid_metric"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	err := RecordSizeHistory(
		ctx, pool, "invalid", obj, 100, nil, "testdb",
	)
	if err == nil {
		t.Fatal(
			"expected CHECK constraint error for invalid " +
				"metric_type, got nil",
		)
	}
}

// --- Test 5 ---

func TestQuerySizeHistory_Empty(t *testing.T) {
	pool, ctx := requireDB(t)

	points, err := QuerySizeHistory(
		ctx, pool, "table", "nonexistent_object_xyz", 30,
	)
	if err != nil {
		t.Fatalf("QuerySizeHistory: %v", err)
	}
	if len(points) != 0 {
		t.Errorf(
			"expected 0 points, got %d", len(points),
		)
	}
}

// --- Test 6 ---

func TestQuerySizeHistory_RoundTrip(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_roundtrip"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	// Insert 5 records with increasing sizes.
	for i := range 5 {
		size := int64((i + 1) * 1000)
		err := RecordSizeHistory(
			ctx, pool, "table", obj, size, nil, "testdb",
		)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	points, err := QuerySizeHistory(
		ctx, pool, "table", obj, 7,
	)
	if err != nil {
		t.Fatalf("QuerySizeHistory: %v", err)
	}

	if len(points) != 5 {
		t.Fatalf("expected 5 points, got %d", len(points))
	}

	// Verify ordering: collected_at ASC means sizes should
	// be non-decreasing (all inserted at ~same time, but
	// serial IDs guarantee order with same timestamps).
	for i := 1; i < len(points); i++ {
		if points[i].CollectedAt.Before(
			points[i-1].CollectedAt,
		) {
			t.Errorf(
				"point %d collected_at (%v) before point %d (%v)",
				i, points[i].CollectedAt,
				i-1, points[i-1].CollectedAt,
			)
		}
	}

	// Verify size values are the ones we inserted.
	for i, p := range points {
		want := int64((i + 1) * 1000)
		if p.SizeBytes != want {
			t.Errorf(
				"point %d size: got %d, want %d",
				i, p.SizeBytes, want,
			)
		}
	}
}

// --- Test 7 ---

func TestQuerySizeHistory_LookbackFilter(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_lookback_filter"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	// Insert a recent record.
	err := RecordSizeHistory(
		ctx, pool, "table", obj, 1000, nil, "testdb",
	)
	if err != nil {
		t.Fatalf("insert recent: %v", err)
	}

	// Insert another record and backdate it to 30 days ago.
	err = RecordSizeHistory(
		ctx, pool, "table", obj, 500, nil, "testdb",
	)
	if err != nil {
		t.Fatalf("insert old: %v", err)
	}

	// Backdate the oldest record (smallest size) to 30 days ago.
	_, err = pool.Exec(ctx,
		`UPDATE sage.size_history
		 SET collected_at = now() - interval '30 days'
		 WHERE object_name = $1 AND size_bytes = 500`, obj,
	)
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Query with 7-day lookback: should only find the recent one.
	points, err := QuerySizeHistory(
		ctx, pool, "table", obj, 7,
	)
	if err != nil {
		t.Fatalf("QuerySizeHistory(7d): %v", err)
	}
	if len(points) != 1 {
		t.Errorf(
			"7-day lookback: expected 1 point, got %d",
			len(points),
		)
	}
	if len(points) == 1 && points[0].SizeBytes != 1000 {
		t.Errorf(
			"expected recent record (1000), got %d",
			points[0].SizeBytes,
		)
	}

	// Query with 60-day lookback: should find both.
	points, err = QuerySizeHistory(
		ctx, pool, "table", obj, 60,
	)
	if err != nil {
		t.Fatalf("QuerySizeHistory(60d): %v", err)
	}
	if len(points) != 2 {
		t.Errorf(
			"60-day lookback: expected 2 points, got %d",
			len(points),
		)
	}
}

// --- Test 8 ---

func TestQuerySizeHistory_MetricTypeFilter(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_metric_filter"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	// Insert records with different metric types for the
	// same object_name.
	for _, mt := range []string{"database", "table", "wal_slot"} {
		err := RecordSizeHistory(
			ctx, pool, mt, obj, 1000, nil, "testdb",
		)
		if err != nil {
			t.Fatalf("insert %s: %v", mt, err)
		}
	}

	// Query for "table" only.
	points, err := QuerySizeHistory(
		ctx, pool, "table", obj, 30,
	)
	if err != nil {
		t.Fatalf("QuerySizeHistory: %v", err)
	}
	if len(points) != 1 {
		t.Errorf(
			"expected 1 point for metric_type=table, got %d",
			len(points),
		)
	}

	// Query for "database" only.
	points, err = QuerySizeHistory(
		ctx, pool, "database", obj, 30,
	)
	if err != nil {
		t.Fatalf("QuerySizeHistory(database): %v", err)
	}
	if len(points) != 1 {
		t.Errorf(
			"expected 1 point for metric_type=database, got %d",
			len(points),
		)
	}
}

// --- Test 9 ---

func TestRecordSizeHistory_LargeSize(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_large_size"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	// 1 PB = 1 << 50
	largeSize := int64(1) << 50

	err := RecordSizeHistory(
		ctx, pool, "database", obj, largeSize, nil, "testdb",
	)
	if err != nil {
		t.Fatalf("RecordSizeHistory: %v", err)
	}

	var gotSize int64
	err = pool.QueryRow(ctx,
		`SELECT size_bytes FROM sage.size_history
		 WHERE object_name = $1`, obj,
	).Scan(&gotSize)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotSize != largeSize {
		t.Errorf(
			"size_bytes: got %d, want %d", gotSize, largeSize,
		)
	}
}

// --- Test 10 ---

func TestQuerySizeHistory_IntegrationWithForecast(t *testing.T) {
	pool, ctx := requireDB(t)
	obj := "test_forecast_integration"
	t.Cleanup(func() { cleanupObject(t, pool, obj) })

	// Insert enough data points with increasing sizes and
	// spread timestamps so the linear regression has real data.
	baseSize := int64(1000)
	growthPerDay := int64(500)
	numPoints := 10

	for i := range numPoints {
		size := baseSize + int64(i)*growthPerDay
		err := RecordSizeHistory(
			ctx, pool, "table", obj, size, nil, "testdb",
		)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}

		// Backdate each record so they are spread across days.
		daysAgo := numPoints - 1 - i
		if daysAgo > 0 {
			_, err = pool.Exec(ctx,
				fmt.Sprintf(
					`UPDATE sage.size_history
					 SET collected_at = now() - interval '%d days'
					 WHERE object_name = $1
					   AND size_bytes = $2`,
					daysAgo,
				), obj, size,
			)
			if err != nil {
				t.Fatalf("backdate %d: %v", i, err)
			}
		}
	}

	// Query the data back.
	points, err := QuerySizeHistory(
		ctx, pool, "table", obj, 30,
	)
	if err != nil {
		t.Fatalf("QuerySizeHistory: %v", err)
	}
	if len(points) != numPoints {
		t.Fatalf(
			"expected %d points, got %d",
			numPoints, len(points),
		)
	}

	// Pass to forecastGrowth and verify a non-nil result.
	capacity := int64(100000)
	fc := forecastGrowth(points, capacity, 3, 0.5)
	if fc == nil {
		t.Fatal("expected non-nil forecast from real data")
	}

	if fc.DataPoints != numPoints {
		t.Errorf(
			"DataPoints: got %d, want %d",
			fc.DataPoints, numPoints,
		)
	}
	if fc.CurrentBytes <= 0 {
		t.Errorf(
			"CurrentBytes: got %d, want positive",
			fc.CurrentBytes,
		)
	}
	if fc.GrowthRateBytes <= 0 {
		t.Errorf(
			"GrowthRateBytes: got %d, want positive",
			fc.GrowthRateBytes,
		)
	}
	if fc.DaysUntilFull <= 0 {
		t.Errorf(
			"DaysUntilFull: got %d, want positive",
			fc.DaysUntilFull,
		)
	}
	if fc.R2 <= 0 {
		t.Errorf("R2: got %f, want positive", fc.R2)
	}
}
