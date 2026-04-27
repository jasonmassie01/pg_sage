package forecaster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// ---------------------------------------------------------------------------
// DB pool setup — mirrors patterns from retention and schema test packages
// ---------------------------------------------------------------------------

func phase2DSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
}

var (
	p2Pool     *pgxpool.Pool
	p2PoolOnce sync.Once
	p2PoolErr  error
)

const destructiveTestLockKey = "pg_sage_test_cross_pkg"

func phase2RequireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	p2PoolOnce.Do(func() {
		dsn := phase2DSN()
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			p2PoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		poolCfg.MaxConns = 2
		p2Pool, p2PoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if p2PoolErr != nil {
			return
		}
		if err := p2Pool.Ping(ctx); err != nil {
			p2PoolErr = fmt.Errorf("ping: %w", err)
			p2Pool.Close()
			p2Pool = nil
			return
		}
		releaseLock, err := lockCrossPackage(ctx, p2Pool)
		if err != nil {
			p2PoolErr = fmt.Errorf("cross-package test lock: %w", err)
			p2Pool.Close()
			p2Pool = nil
			return
		}
		defer releaseLock()
		if err := schema.Bootstrap(ctx, p2Pool); err != nil {
			p2PoolErr = fmt.Errorf("bootstrap: %w", err)
			p2Pool.Close()
			p2Pool = nil
			return
		}
		schema.ReleaseAdvisoryLock(ctx, p2Pool)
		releasePoolAdvisoryLocks(ctx, p2Pool)
	})
	if p2PoolErr != nil {
		t.Skipf("database unavailable: %v", p2PoolErr)
	}
	serializeAcrossPackages(t, ctx, p2Pool)
	return p2Pool, ctx
}

func serializeAcrossPackages(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	releaseLock, err := lockCrossPackage(ctx, pool)
	if err != nil {
		t.Fatalf("acquire cross-package test lock: %v", err)
	}
	t.Cleanup(releaseLock)
}

func lockCrossPackage(
	ctx context.Context, pool *pgxpool.Pool,
) (func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn for cross-pkg lock: %w", err)
	}
	_, err = conn.Exec(ctx,
		"SELECT pg_advisory_lock(hashtext($1))",
		destructiveTestLockKey)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return func() {
		_, _ = conn.Exec(context.Background(),
			"SELECT pg_advisory_unlock(hashtext($1))",
			destructiveTestLockKey)
		conn.Release()
	}, nil
}

func releasePoolAdvisoryLocks(ctx context.Context, pool *pgxpool.Pool) {
	total := int(pool.Stat().TotalConns())
	conns := make([]*pgxpool.Conn, 0, total)
	for i := 0; i < total; i++ {
		qctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		conn, err := pool.Acquire(qctx)
		cancel()
		if err != nil {
			break
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock_all()")
		conn.Release()
	}
}

// cleanupSnapshots removes test-seeded rows by category prefix.
func cleanupSnapshots(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	prefix string,
) {
	t.Helper()
	_, err := pool.Exec(ctx,
		"DELETE FROM sage.snapshots WHERE category LIKE $1",
		prefix+"%",
	)
	if err != nil {
		t.Fatalf("cleanup snapshots: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helper: seed system snapshots
// ---------------------------------------------------------------------------

type systemSnapshotData struct {
	DBSizeBytes      int64   `json:"db_size_bytes"`
	ActiveBackends   int     `json:"active_backends"`
	TotalBackends    int     `json:"total_backends"`
	MaxConnections   int     `json:"max_connections"`
	CacheHitRatio    float64 `json:"cache_hit_ratio"`
	TotalCheckpoints int64   `json:"total_checkpoints"`
}

func seedSystemSnapshots(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	days int,
	fill func(dayOffset int) systemSnapshotData,
) {
	t.Helper()
	for i := 0; i < days; i++ {
		ts := time.Now().AddDate(0, 0, -days+i)
		d := fill(i)
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal system data: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.snapshots (collected_at, category, data)
			 VALUES ($1, 'system', $2::jsonb)`,
			ts, string(raw),
		)
		if err != nil {
			t.Fatalf("seed system snapshot day %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Helper: seed query snapshots
// ---------------------------------------------------------------------------

type queryElem struct {
	QueryID int64 `json:"queryid"`
	Calls   int64 `json:"calls"`
}

func seedQuerySnapshots(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	days int,
	fill func(dayOffset int) []queryElem,
) {
	t.Helper()
	for i := 0; i < days; i++ {
		ts := time.Now().AddDate(0, 0, -days+i)
		elems := fill(i)
		raw, err := json.Marshal(elems)
		if err != nil {
			t.Fatalf("marshal query data: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.snapshots (collected_at, category, data)
			 VALUES ($1, 'queries', $2::jsonb)`,
			ts, string(raw),
		)
		if err != nil {
			t.Fatalf("seed query snapshot day %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Helper: seed sequence snapshots
// ---------------------------------------------------------------------------

type seqElem struct {
	SchemaName   string  `json:"schemaname"`
	SequenceName string  `json:"sequencename"`
	PctUsed      float64 `json:"pct_used"`
	MaxValue     int64   `json:"max_value"`
}

func seedSeqSnapshots(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	days int,
	fill func(dayOffset int) []seqElem,
) {
	t.Helper()
	for i := 0; i < days; i++ {
		ts := time.Now().AddDate(0, 0, -days+i)
		elems := fill(i)
		raw, err := json.Marshal(elems)
		if err != nil {
			t.Fatalf("marshal seq data: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.snapshots (collected_at, category, data)
			 VALUES ($1, 'sequences', $2::jsonb)`,
			ts, string(raw),
		)
		if err != nil {
			t.Fatalf("seed seq snapshot day %d: %v", i, err)
		}
	}
}

// ===========================================================================
// QueryDailySystemAggs tests
// ===========================================================================

func TestPhase2_QueryDailySystemAggs_WithData(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	// Clean up any prior test data and seed fresh.
	cleanupSnapshots(t, pool, ctx, "system")
	seedSystemSnapshots(t, pool, ctx, 10, func(i int) systemSnapshotData {
		return systemSnapshotData{
			DBSizeBytes:      int64(100e9) + int64(i)*int64(1e9),
			ActiveBackends:   10 + i,
			TotalBackends:    20 + i,
			MaxConnections:   200,
			CacheHitRatio:    99.5 - float64(i)*0.1,
			TotalCheckpoints: int64(i) * 50,
		}
	})

	aggs, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs: %v", err)
	}
	if len(aggs) == 0 {
		t.Fatal("expected at least one daily system aggregate, got 0")
	}

	// Verify the returned struct fields are populated.
	for i, a := range aggs {
		if a.Day.IsZero() {
			t.Errorf("agg[%d].Day is zero", i)
		}
		if a.AvgDBSizeBytes <= 0 {
			t.Errorf("agg[%d].AvgDBSizeBytes = %f, want > 0",
				i, a.AvgDBSizeBytes)
		}
		if a.MaxDBSizeBytes <= 0 {
			t.Errorf("agg[%d].MaxDBSizeBytes = %f, want > 0",
				i, a.MaxDBSizeBytes)
		}
		if a.MaxConnections <= 0 {
			t.Errorf("agg[%d].MaxConnections = %f, want > 0",
				i, a.MaxConnections)
		}
	}

	// Cleanup after test.
	cleanupSnapshots(t, pool, ctx, "system")
}

func TestPhase2_QueryDailySystemAggs_EmptyTable(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	// Ensure no system rows exist.
	cleanupSnapshots(t, pool, ctx, "system")

	aggs, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs on empty table: %v", err)
	}
	if len(aggs) != 0 {
		t.Errorf("expected 0 aggs from empty table, got %d", len(aggs))
	}
}

func TestPhase2_QueryDailySystemAggs_SingleDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "system")
	seedSystemSnapshots(t, pool, ctx, 1, func(_ int) systemSnapshotData {
		return systemSnapshotData{
			DBSizeBytes:      50e9,
			ActiveBackends:   5,
			TotalBackends:    10,
			MaxConnections:   100,
			CacheHitRatio:    99.9,
			TotalCheckpoints: 10,
		}
	})

	aggs, err := QueryDailySystemAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs single day: %v", err)
	}
	if len(aggs) != 1 {
		t.Fatalf("expected 1 agg for single day, got %d", len(aggs))
	}
	a := aggs[0]
	if a.AvgDBSizeBytes != 50e9 {
		t.Errorf("AvgDBSizeBytes = %f, want 50e9", a.AvgDBSizeBytes)
	}
	if a.MaxActiveBackends != 5 {
		t.Errorf("MaxActiveBackends = %f, want 5", a.MaxActiveBackends)
	}
	if a.MaxConnections != 100 {
		t.Errorf("MaxConnections = %f, want 100", a.MaxConnections)
	}

	cleanupSnapshots(t, pool, ctx, "system")
}

func TestPhase2_QueryDailySystemAggs_LookbackFilters(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "system")
	// Seed 20 days of data.
	seedSystemSnapshots(t, pool, ctx, 20, func(i int) systemSnapshotData {
		return systemSnapshotData{
			DBSizeBytes:      int64(10e9),
			ActiveBackends:   1,
			TotalBackends:    2,
			MaxConnections:   100,
			CacheHitRatio:    99.0,
			TotalCheckpoints: int64(i),
		}
	})

	// Query with lookback=10 should return fewer rows than lookback=30.
	aggsShort, err := QueryDailySystemAggs(ctx, pool, 10)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs lookback=10: %v", err)
	}
	aggsLong, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs lookback=30: %v", err)
	}
	if len(aggsLong) < len(aggsShort) {
		t.Errorf("longer lookback returned fewer rows: %d < %d",
			len(aggsLong), len(aggsShort))
	}

	cleanupSnapshots(t, pool, ctx, "system")
}

// ===========================================================================
// QueryDailyQueryAggs tests
// ===========================================================================

func TestPhase2_QueryDailyQueryAggs_WithData(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "queries")
	seedQuerySnapshots(t, pool, ctx, 10, func(i int) []queryElem {
		return []queryElem{
			{QueryID: 1001, Calls: int64(100 + i*10)},
			{QueryID: 1002, Calls: int64(200 + i*5)},
		}
	})

	aggs, err := QueryDailyQueryAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailyQueryAggs: %v", err)
	}
	if len(aggs) == 0 {
		t.Fatal("expected at least one daily query aggregate, got 0")
	}

	for i, a := range aggs {
		if a.Day.IsZero() {
			t.Errorf("agg[%d].Day is zero", i)
		}
		if a.TotalCalls <= 0 {
			t.Errorf("agg[%d].TotalCalls = %f, want > 0",
				i, a.TotalCalls)
		}
	}

	cleanupSnapshots(t, pool, ctx, "queries")
}

func TestPhase2_QueryDailyQueryAggs_EmptyTable(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "queries")

	aggs, err := QueryDailyQueryAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailyQueryAggs on empty table: %v", err)
	}
	if len(aggs) != 0 {
		t.Errorf("expected 0 aggs from empty table, got %d", len(aggs))
	}
}

func TestPhase2_QueryDailyQueryAggs_SingleDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "queries")
	seedQuerySnapshots(t, pool, ctx, 1, func(_ int) []queryElem {
		return []queryElem{
			{QueryID: 2001, Calls: 500},
		}
	})

	aggs, err := QueryDailyQueryAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailyQueryAggs single day: %v", err)
	}
	if len(aggs) != 1 {
		t.Fatalf("expected 1 agg for single day, got %d", len(aggs))
	}
	if aggs[0].TotalCalls != 500 {
		t.Errorf("TotalCalls = %f, want 500", aggs[0].TotalCalls)
	}

	cleanupSnapshots(t, pool, ctx, "queries")
}

func TestPhase2_QueryDailyQueryAggs_MultipleQueryIDs(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "queries")
	// Seed 1 day with 3 different query IDs.
	seedQuerySnapshots(t, pool, ctx, 1, func(_ int) []queryElem {
		return []queryElem{
			{QueryID: 3001, Calls: 100},
			{QueryID: 3002, Calls: 200},
			{QueryID: 3003, Calls: 300},
		}
	})

	aggs, err := QueryDailyQueryAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailyQueryAggs multi-qid: %v", err)
	}
	if len(aggs) != 1 {
		t.Fatalf("expected 1 daily aggregate, got %d", len(aggs))
	}
	// TotalCalls should be sum of max(calls) per queryid = 100+200+300 = 600.
	if aggs[0].TotalCalls != 600 {
		t.Errorf("TotalCalls = %f, want 600", aggs[0].TotalCalls)
	}

	cleanupSnapshots(t, pool, ctx, "queries")
}

// ===========================================================================
// QueryDailySeqAggs tests
// ===========================================================================

func TestPhase2_QueryDailySeqAggs_WithData(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "sequences")
	seedSeqSnapshots(t, pool, ctx, 10, func(i int) []seqElem {
		return []seqElem{
			{
				SchemaName:   "public",
				SequenceName: "orders_id_seq",
				PctUsed:      50.0 + float64(i)*2.0,
				MaxValue:     2147483647,
			},
		}
	})

	aggs, err := QueryDailySeqAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySeqAggs: %v", err)
	}
	if len(aggs) == 0 {
		t.Fatal("expected at least one daily seq aggregate, got 0")
	}

	for i, a := range aggs {
		if a.Day.IsZero() {
			t.Errorf("agg[%d].Day is zero", i)
		}
		if a.SeqName != "public.orders_id_seq" {
			t.Errorf("agg[%d].SeqName = %q, want public.orders_id_seq",
				i, a.SeqName)
		}
		if a.PctUsed <= 0 {
			t.Errorf("agg[%d].PctUsed = %f, want > 0", i, a.PctUsed)
		}
		if a.MaxValue != 2147483647 {
			t.Errorf("agg[%d].MaxValue = %d, want 2147483647",
				i, a.MaxValue)
		}
	}

	cleanupSnapshots(t, pool, ctx, "sequences")
}

func TestPhase2_QueryDailySeqAggs_EmptyTable(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "sequences")

	aggs, err := QueryDailySeqAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySeqAggs on empty table: %v", err)
	}
	if len(aggs) != 0 {
		t.Errorf("expected 0 aggs from empty table, got %d", len(aggs))
	}
}

func TestPhase2_QueryDailySeqAggs_SingleDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "sequences")
	seedSeqSnapshots(t, pool, ctx, 1, func(_ int) []seqElem {
		return []seqElem{
			{
				SchemaName:   "myschema",
				SequenceName: "users_id_seq",
				PctUsed:      25.5,
				MaxValue:     9223372036854775807,
			},
		}
	})

	aggs, err := QueryDailySeqAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailySeqAggs single day: %v", err)
	}
	if len(aggs) != 1 {
		t.Fatalf("expected 1 agg for single day, got %d", len(aggs))
	}
	if aggs[0].SeqName != "myschema.users_id_seq" {
		t.Errorf("SeqName = %q, want myschema.users_id_seq",
			aggs[0].SeqName)
	}
	if aggs[0].PctUsed != 25.5 {
		t.Errorf("PctUsed = %f, want 25.5", aggs[0].PctUsed)
	}

	cleanupSnapshots(t, pool, ctx, "sequences")
}

func TestPhase2_QueryDailySeqAggs_MultipleSequences(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "sequences")
	seedSeqSnapshots(t, pool, ctx, 3, func(i int) []seqElem {
		return []seqElem{
			{
				SchemaName:   "public",
				SequenceName: "seq_a",
				PctUsed:      10.0 + float64(i),
				MaxValue:     1000000,
			},
			{
				SchemaName:   "public",
				SequenceName: "seq_b",
				PctUsed:      20.0 + float64(i)*2,
				MaxValue:     2000000,
			},
		}
	})

	aggs, err := QueryDailySeqAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailySeqAggs multi-seq: %v", err)
	}

	// Should have rows for both sequences across days.
	seqNames := make(map[string]int)
	for _, a := range aggs {
		seqNames[a.SeqName]++
	}
	if seqNames["public.seq_a"] == 0 {
		t.Error("expected rows for public.seq_a")
	}
	if seqNames["public.seq_b"] == 0 {
		t.Error("expected rows for public.seq_b")
	}

	cleanupSnapshots(t, pool, ctx, "sequences")
}

// ===========================================================================
// Forecast (full integration)
// ===========================================================================

func TestPhase2_Forecast_EmptyDB(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	// Clean all snapshot categories to ensure empty state.
	cleanupSnapshots(t, pool, ctx, "system")
	cleanupSnapshots(t, pool, ctx, "queries")
	cleanupSnapshots(t, pool, ctx, "sequences")

	var logMsgs []string
	logFn := func(level, msg string, args ...any) {
		logMsgs = append(logMsgs,
			fmt.Sprintf("[%s] %s", level, fmt.Sprintf(msg, args...)))
	}

	cfg := ForecasterConfig{
		Enabled:              true,
		LookbackDays:         30,
		DiskWarnGrowthGBDay:  1.0,
		ConnectionWarnPct:    80,
		CacheWarnThreshold:   0.90,
		SequenceWarnDays:     90,
		SequenceCriticalDays: 30,
	}
	f := New(pool, cfg, logFn)

	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	// No data means no findings (all rule functions need minDataPoints).
	if len(findings) != 0 {
		t.Errorf("expected 0 findings on empty DB, got %d", len(findings))
	}
}

func TestPhase2_Forecast_WithSystemDataOnly(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "system")
	cleanupSnapshots(t, pool, ctx, "queries")
	cleanupSnapshots(t, pool, ctx, "sequences")

	// Seed 14 days of rapidly growing system data to trigger
	// disk growth findings.
	seedSystemSnapshots(t, pool, ctx, 14, func(i int) systemSnapshotData {
		return systemSnapshotData{
			DBSizeBytes:      int64(100e9) + int64(i)*int64(20e9),
			ActiveBackends:   10 + i*5,
			TotalBackends:    20 + i*5,
			MaxConnections:   200,
			CacheHitRatio:    99.0,
			TotalCheckpoints: int64(i) * 100,
		}
	})

	var logMsgs []string
	logFn := func(level, msg string, args ...any) {
		logMsgs = append(logMsgs,
			fmt.Sprintf("[%s] %s", level, fmt.Sprintf(msg, args...)))
	}

	cfg := ForecasterConfig{
		Enabled:              true,
		LookbackDays:         30,
		DiskWarnGrowthGBDay:  1.0,
		ConnectionWarnPct:    80,
		CacheWarnThreshold:   0.90,
		SequenceWarnDays:     90,
		SequenceCriticalDays: 30,
	}
	f := New(pool, cfg, logFn)

	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}

	// We should get at least a disk growth finding since we're
	// growing at 20 GB/day (> threshold of 1 GB/day).
	foundDisk := false
	for _, finding := range findings {
		if finding.Category == "forecast_disk_growth" {
			foundDisk = true
			if finding.Severity != "warning" {
				t.Errorf("disk growth severity = %q, want warning",
					finding.Severity)
			}
			if finding.ObjectType != "database" {
				t.Errorf("disk growth objectType = %q, want database",
					finding.ObjectType)
			}
		}
	}
	if !foundDisk {
		t.Error("expected forecast_disk_growth finding")
	}

	cleanupSnapshots(t, pool, ctx, "system")
}

func TestPhase2_Forecast_InsufficientData(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "system")
	cleanupSnapshots(t, pool, ctx, "queries")
	cleanupSnapshots(t, pool, ctx, "sequences")

	// Seed only 3 days — below minDataPoints (7) for all rules.
	seedSystemSnapshots(t, pool, ctx, 3, func(i int) systemSnapshotData {
		return systemSnapshotData{
			DBSizeBytes:      int64(100e9) + int64(i)*int64(50e9),
			ActiveBackends:   10 + i*10,
			TotalBackends:    20,
			MaxConnections:   200,
			CacheHitRatio:    99.0,
			TotalCheckpoints: int64(i) * 500,
		}
	})

	logFn := func(level, msg string, args ...any) {}

	cfg := ForecasterConfig{
		Enabled:              true,
		LookbackDays:         30,
		DiskWarnGrowthGBDay:  0.1,
		ConnectionWarnPct:    50,
		CacheWarnThreshold:   0.99,
		SequenceWarnDays:     365,
		SequenceCriticalDays: 180,
	}
	f := New(pool, cfg, logFn)

	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	// All rules require >= 7 data points so no findings expected.
	if len(findings) != 0 {
		t.Errorf("expected 0 findings with insufficient data, got %d",
			len(findings))
	}

	cleanupSnapshots(t, pool, ctx, "system")
}

func TestPhase2_Forecast_AllCategories(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "system")
	cleanupSnapshots(t, pool, ctx, "queries")
	cleanupSnapshots(t, pool, ctx, "sequences")

	// Seed system data: 14 days of growing disk and connections,
	// declining cache hit, high checkpoints.
	seedSystemSnapshots(t, pool, ctx, 14, func(i int) systemSnapshotData {
		return systemSnapshotData{
			DBSizeBytes:      int64(100e9) + int64(i)*int64(20e9),
			ActiveBackends:   50 + i*10,
			TotalBackends:    60 + i*10,
			MaxConnections:   200,
			CacheHitRatio:    95.0 - float64(i)*3.0,
			TotalCheckpoints: int64(i) * 400,
		}
	})

	// Seed query data: 14 days with doubling volume second week.
	seedQuerySnapshots(t, pool, ctx, 14, func(i int) []queryElem {
		calls := int64(1000)
		if i >= 7 {
			calls = 3000 // 200% growth
		}
		return []queryElem{
			{QueryID: 5001, Calls: calls},
		}
	})

	// Seed sequence data: 14 days of rising pct_used.
	seedSeqSnapshots(t, pool, ctx, 14, func(i int) []seqElem {
		return []seqElem{
			{
				SchemaName:   "public",
				SequenceName: "test_phase2_seq",
				PctUsed:      80.0 + float64(i)*1.2,
				MaxValue:     2147483647,
			},
		}
	})

	logFn := func(level, msg string, args ...any) {}

	cfg := ForecasterConfig{
		Enabled:              true,
		LookbackDays:         30,
		DiskWarnGrowthGBDay:  1.0,
		ConnectionWarnPct:    80,
		CacheWarnThreshold:   0.90,
		SequenceWarnDays:     90,
		SequenceCriticalDays: 30,
	}
	f := New(pool, cfg, logFn)

	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}

	// Verify we got at least some findings from the seeded data.
	if len(findings) == 0 {
		t.Error("expected at least one finding from seeded data")
	}

	// Categorize findings.
	categories := make(map[string]int)
	for _, f := range findings {
		categories[f.Category]++
		// Verify every finding has required fields populated.
		if f.Title == "" {
			t.Errorf("finding %q has empty Title", f.Category)
		}
		if f.Severity == "" {
			t.Errorf("finding %q has empty Severity", f.Category)
		}
		if f.ObjectType == "" {
			t.Errorf("finding %q has empty ObjectType", f.Category)
		}
		if f.Detail == nil {
			t.Errorf("finding %q has nil Detail", f.Category)
		}
		if f.Recommendation == "" {
			t.Errorf("finding %q has empty Recommendation",
				f.Category)
		}
	}

	t.Logf("findings by category: %v", categories)

	cleanupSnapshots(t, pool, ctx, "system")
	cleanupSnapshots(t, pool, ctx, "queries")
	cleanupSnapshots(t, pool, ctx, "sequences")
}

func TestPhase2_Forecast_CancelledContext(t *testing.T) {
	pool, _ := phase2RequireDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	logFn := func(level, msg string, args ...any) {}
	cfg := ForecasterConfig{
		Enabled:      true,
		LookbackDays: 30,
	}
	f := New(pool, cfg, logFn)

	// Should not panic; errors logged via logFn.
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast with cancelled ctx: %v", err)
	}
	// All three queries will fail due to cancelled context,
	// so no findings expected (errors logged via logFn).
	if len(findings) != 0 {
		t.Errorf("expected 0 findings with cancelled ctx, got %d",
			len(findings))
	}
}

// ===========================================================================
// Aggregation correctness: multiple snapshots per day
// ===========================================================================

func TestPhase2_QueryDailySystemAggs_AggregatesMultiplePerDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "system")

	// Insert 3 snapshots on the same day with different values.
	today := time.Now()
	for _, size := range []int64{10e9, 20e9, 30e9} {
		d := systemSnapshotData{
			DBSizeBytes:      size,
			ActiveBackends:   5,
			TotalBackends:    10,
			MaxConnections:   100,
			CacheHitRatio:    99.0,
			TotalCheckpoints: 0,
		}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.snapshots (collected_at, category, data)
			 VALUES ($1, 'system', $2::jsonb)`,
			today, string(raw),
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	aggs, err := QueryDailySystemAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs: %v", err)
	}
	if len(aggs) != 1 {
		t.Fatalf("expected 1 day aggregate, got %d", len(aggs))
	}

	// avg(10e9, 20e9, 30e9) = 20e9
	expectedAvg := 20e9
	if aggs[0].AvgDBSizeBytes != expectedAvg {
		t.Errorf("AvgDBSizeBytes = %f, want %f",
			aggs[0].AvgDBSizeBytes, expectedAvg)
	}
	// max(10e9, 20e9, 30e9) = 30e9
	expectedMax := 30e9
	if aggs[0].MaxDBSizeBytes != expectedMax {
		t.Errorf("MaxDBSizeBytes = %f, want %f",
			aggs[0].MaxDBSizeBytes, expectedMax)
	}

	cleanupSnapshots(t, pool, ctx, "system")
}

func TestPhase2_QueryDailyQueryAggs_DedupsPerQueryID(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "queries")

	// Insert 2 snapshots on same day with same queryid but
	// different call counts. The SQL takes max(calls) per queryid.
	today := time.Now()
	for _, calls := range []int64{100, 300} {
		elems := []queryElem{{QueryID: 7001, Calls: calls}}
		raw, err := json.Marshal(elems)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.snapshots (collected_at, category, data)
			 VALUES ($1, 'queries', $2::jsonb)`,
			today, string(raw),
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	aggs, err := QueryDailyQueryAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailyQueryAggs: %v", err)
	}
	if len(aggs) != 1 {
		t.Fatalf("expected 1 day aggregate, got %d", len(aggs))
	}
	// max(calls) for queryid 7001 = 300.
	if aggs[0].TotalCalls != 300 {
		t.Errorf("TotalCalls = %f, want 300 (max of 100, 300)",
			aggs[0].TotalCalls)
	}

	cleanupSnapshots(t, pool, ctx, "queries")
}

func TestPhase2_QueryDailySeqAggs_MaxAcrossSameDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	cleanupSnapshots(t, pool, ctx, "sequences")

	// Insert 2 snapshots same day, same sequence, different pct_used.
	today := time.Now()
	for _, pct := range []float64{40.0, 60.0} {
		elems := []seqElem{
			{
				SchemaName:   "public",
				SequenceName: "agg_test_seq",
				PctUsed:      pct,
				MaxValue:     1000000,
			},
		}
		raw, err := json.Marshal(elems)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.snapshots (collected_at, category, data)
			 VALUES ($1, 'sequences', $2::jsonb)`,
			today, string(raw),
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	aggs, err := QueryDailySeqAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailySeqAggs: %v", err)
	}

	found := false
	for _, a := range aggs {
		if a.SeqName == "public.agg_test_seq" {
			found = true
			// max(40, 60) = 60
			if a.PctUsed != 60.0 {
				t.Errorf("PctUsed = %f, want 60.0", a.PctUsed)
			}
		}
	}
	if !found {
		t.Error("expected row for public.agg_test_seq")
	}

	cleanupSnapshots(t, pool, ctx, "sequences")
}
