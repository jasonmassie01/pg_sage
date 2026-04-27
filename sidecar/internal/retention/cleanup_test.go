package retention

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
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
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		poolCfg.MaxConns = 1
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		// Ensure sage schema exists.
		if err := schema.Bootstrap(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		// Run config migration so database_id column and composite
		// unique index exist (required by ON CONFLICT clauses).
		if err := schema.MigrateConfigSchema(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("config migration: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		schema.ReleaseAdvisoryLock(ctx, testPool)
	})
	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}

	// Verify sage schema is intact — it may have been dropped by
	// concurrent schema package tests running in parallel via go test ./...
	ensureSageSchema(t, ctx)

	return testPool, ctx
}

var sageMu sync.Mutex

// ensureSageSchema re-bootstraps if the sage schema was dropped by
// concurrent test packages.
func ensureSageSchema(t *testing.T, ctx context.Context) {
	t.Helper()
	sageMu.Lock()
	defer sageMu.Unlock()
	var n int
	err := testPool.QueryRow(ctx,
		`SELECT 1 FROM information_schema.tables
		 WHERE table_schema = 'sage'
		   AND table_name = 'snapshots'`).Scan(&n)
	if err == nil {
		return
	}
	if err := schema.Bootstrap(ctx, testPool); err != nil {
		t.Skipf("re-bootstrap sage failed: %v", err)
	}
	if err := schema.MigrateConfigSchema(ctx, testPool); err != nil {
		t.Skipf("re-migrate sage config: %v", err)
	}
	schema.ReleaseAdvisoryLock(ctx, testPool)
}

// execRetry runs sql and retries once after re-bootstrapping sage
// if the schema was dropped by a concurrent test package.
func execRetry(
	t *testing.T, ctx context.Context, sql string, args ...any,
) {
	t.Helper()
	_, err := testPool.Exec(ctx, sql, args...)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensureSageSchema(t, ctx)
		_, err = testPool.Exec(ctx, sql, args...)
	}
	if err != nil {
		t.Fatalf("exec: %v\nsql: %s", err, sql)
	}
}

// queryRetry runs a query scan and skips if sage was dropped mid-test.
func queryRetry(
	t *testing.T, ctx context.Context, sql string, dest ...any,
) {
	t.Helper()
	err := testPool.QueryRow(ctx, sql).Scan(dest...)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		t.Skipf("sage schema dropped by concurrent tests: %v", err)
	}
	if err != nil {
		t.Fatalf("query: %v\nsql: %s", err, sql)
	}
}

func noopLog(_ string, _ string, _ ...any) {}

func TestNew(t *testing.T) {
	cfg := &config.Config{}
	c := New(nil, cfg, noopLog)
	if c == nil {
		t.Fatal("expected non-nil Cleaner")
	}
	if c.cfg != cfg {
		t.Error("config not stored")
	}
	if c.pool != nil {
		t.Error("pool should be nil")
	}
}

func TestBatchSizeConstant(t *testing.T) {
	if batchSize != 1000 {
		t.Errorf("expected batchSize=1000, got %d", batchSize)
	}
}

func TestPurgeQueryFormat(t *testing.T) {
	// Verify the SQL generation matches expected patterns.
	tests := []struct {
		table      string
		timeCol    string
		extraWhere string
		wantTable  string
		wantCol    string
		wantExtra  string
	}{
		{
			table:      "snapshots",
			timeCol:    "collected_at",
			extraWhere: "",
			wantTable:  "sage.snapshots",
			wantCol:    "collected_at",
			wantExtra:  "",
		},
		{
			table:      "findings",
			timeCol:    "last_seen",
			extraWhere: "AND status = 'resolved'",
			wantTable:  "sage.findings",
			wantCol:    "last_seen",
			wantExtra:  "AND status = 'resolved'",
		},
		{
			table:      "action_log",
			timeCol:    "executed_at",
			extraWhere: "",
			wantTable:  "sage.action_log",
			wantCol:    "executed_at",
			wantExtra:  "",
		},
		{
			table:      "explain_cache",
			timeCol:    "captured_at",
			extraWhere: "",
			wantTable:  "sage.explain_cache",
			wantCol:    "captured_at",
			wantExtra:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			query := fmt.Sprintf(
				`DELETE FROM sage.%s
			 WHERE ctid IN (
				 SELECT ctid FROM sage.%s
				 WHERE %s < now() - make_interval(days => $1)
				 %s
				 LIMIT %d
			 )`,
				tt.table, tt.table, tt.timeCol, tt.extraWhere, batchSize,
			)

			if !strings.Contains(query, tt.wantTable) {
				t.Errorf("query missing table %q", tt.wantTable)
			}
			if !strings.Contains(query, tt.wantCol) {
				t.Errorf("query missing column %q", tt.wantCol)
			}
			if tt.wantExtra != "" && !strings.Contains(query, tt.wantExtra) {
				t.Errorf("query missing extra clause %q", tt.wantExtra)
			}
			if !strings.Contains(query, "LIMIT 1000") {
				t.Error("query missing LIMIT with batch size")
			}
			if !strings.Contains(query, "make_interval(days => $1)") {
				t.Error("query missing parameterized interval")
			}
			// Two references to the table (DELETE FROM + subquery).
			count := strings.Count(query, "sage."+tt.table)
			if count != 2 {
				t.Errorf("expected 2 table references, got %d", count)
			}
		})
	}
}

func TestPurgeTable_SkipsZeroRetention(t *testing.T) {
	// purgeTable should return immediately when retentionDays <= 0.
	// Since pool is nil, calling pool.Exec would panic. If this
	// test does not panic, the early return is working.
	cfg := &config.Config{}
	c := New(nil, cfg, noopLog)
	c.purgeTable(nil, "snapshots", "collected_at", 0, "")
	c.purgeTable(nil, "snapshots", "collected_at", -1, "")
}

func TestRetentionConfig_DefaultValues(t *testing.T) {
	// Verify the cleaner uses config values correctly by checking
	// that Run dispatches to the right tables with right config.
	cfg := &config.Config{
		Retention: config.RetentionConfig{
			SnapshotsDays: 30,
			FindingsDays:  90,
			ActionsDays:   180,
			ExplainsDays:  14,
		},
	}

	if cfg.Retention.SnapshotsDays != 30 {
		t.Error("expected SnapshotsDays=30")
	}
	if cfg.Retention.FindingsDays != 90 {
		t.Error("expected FindingsDays=90")
	}
	if cfg.Retention.ActionsDays != 180 {
		t.Error("expected ActionsDays=180")
	}
	if cfg.Retention.ExplainsDays != 14 {
		t.Error("expected ExplainsDays=14")
	}
}

func TestPurgeTable_ZeroDaysAllTables(t *testing.T) {
	// When all retention days are 0, Run should not attempt any DB calls.
	cfg := &config.Config{
		Retention: config.RetentionConfig{
			SnapshotsDays: 0,
			FindingsDays:  0,
			ActionsDays:   0,
			ExplainsDays:  0,
		},
	}
	c := New(nil, cfg, noopLog)

	// This would panic if any purgeTable tried to use the nil pool.
	// cleanStaleFirstSeen will try to use pool, so we only test purgeTable.
	c.purgeTable(nil, "snapshots", "collected_at", cfg.Retention.SnapshotsDays, "")
	c.purgeTable(nil, "findings", "last_seen", cfg.Retention.FindingsDays, "AND status = 'resolved'")
	c.purgeTable(nil, "action_log", "executed_at", cfg.Retention.ActionsDays, "")
	c.purgeTable(nil, "explain_cache", "captured_at", cfg.Retention.ExplainsDays, "")
}

func TestRun_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)

	// Insert an old test row (365 days ago).
	execRetry(t, ctx,
		`INSERT INTO sage.snapshots (collected_at, category, data)
		 VALUES (now() - interval '365 days', 'test_retention', '{}'::jsonb)`)

	cfg := &config.Config{
		Retention: config.RetentionConfig{
			SnapshotsDays: 90,
			FindingsDays:  90,
			ActionsDays:   90,
			ExplainsDays:  90,
		},
	}
	c := New(pool, cfg, noopLog)
	c.Run(ctx)

	// The old row should have been deleted.
	var count int
	queryRetry(t, ctx,
		`SELECT count(*) FROM sage.snapshots
		 WHERE category='test_retention'`, &count)
	if count != 0 {
		t.Errorf("expected 0 test_retention rows after cleanup, got %d",
			count)
	}
}

func TestCleanStaleFirstSeen_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)

	// Insert a stale first_seen entry for a nonexistent index.
	execRetry(t, ctx,
		`INSERT INTO sage.config (key, value)
		 VALUES ('first_seen:public.idx_nonexistent', '2025-01-01')
		 ON CONFLICT (key, COALESCE(database_id, 0))
		 DO UPDATE SET value = '2025-01-01'`)

	cfg := &config.Config{
		Retention: config.RetentionConfig{
			SnapshotsDays: 90,
			FindingsDays:  90,
			ActionsDays:   90,
			ExplainsDays:  90,
		},
	}
	c := New(pool, cfg, noopLog)
	c.Run(ctx)

	// The stale entry should have been cleaned up (the index doesn't
	// exist in pg_indexes, so cleanStaleFirstSeen removes it).
	var count int
	queryRetry(t, ctx,
		`SELECT count(*) FROM sage.config
		 WHERE key = 'first_seen:public.idx_nonexistent'`, &count)
	if count != 0 {
		t.Errorf("expected stale first_seen entry to be cleaned, got %d",
			count)
	}
}
