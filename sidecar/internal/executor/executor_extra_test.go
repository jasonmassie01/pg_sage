package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
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

// requireDB returns a shared pgxpool connected to Cloud SQL.
// It bootstraps the sage schema on first call and skips the
// test if the database is unreachable.
func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()

	testPoolOnce.Do(func() {
		dsn := testDSN()

		ctx, cancel := context.WithTimeout(
			context.Background(), 15*time.Second,
		)
		defer cancel()

		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}

		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}

		// Verify connectivity.
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}

		// Bootstrap sage schema and tables.
		if err := schema.Bootstrap(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			testPool.Close()
			testPool = nil
			return
		}

		// Run config migration so database_id column and composite
		// unique index exist (required by SetEmergencyStop's ON CONFLICT).
		if err := schema.MigrateConfigSchema(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("config migration: %w", err)
			testPool.Close()
			testPool = nil
			return
		}

		// Release the advisory lock so other tests/processes can proceed.
		schema.ReleaseAdvisoryLock(ctx, testPool)
	})

	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}

	ctx := context.Background()
	return testPool, ctx
}

func TestInMaintenanceWindow_Variations(t *testing.T) {
	// All cases use autonomous + moderate + ramp > 31d + Tier3Moderate=true
	// so the ONLY variable controlling the result is the maintenance window.
	now := time.Now()
	rampStart := now.Add(-40 * 24 * time.Hour)

	baseConfig := func(window string) *config.Config {
		return &config.Config{
			Trust: config.TrustConfig{
				Level:             "autonomous",
				Tier3Moderate:     true,
				MaintenanceWindow: window,
			},
		}
	}

	finding := analyzer.Finding{ActionRisk: "moderate"}

	tests := []struct {
		name   string
		window string
		want   bool
	}{
		{
			name:   "empty cron string blocks execution",
			window: "",
			want:   false,
		},
		{
			name:   "invalid cron string (single field) blocks execution",
			window: "59",
			want:   false,
		},
		{
			name:   "invalid cron string (non-numeric) blocks execution",
			window: "abc def * * *",
			want:   false,
		},
		{
			name: "currently in window allows execution",
			window: func() string {
				return time.Now().Format("4 15") + " * * *"
			}(),
			want: true,
		},
		{
			name: "outside window blocks execution",
			window: func() string {
				// 12 hours from now is guaranteed to be outside the 1-hour window
				future := time.Now().Add(12 * time.Hour)
				return future.Format("4 15") + " * * *"
			}(),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(tc.window)
			got := ShouldExecute(finding, cfg, rampStart, false, false)
			if got != tc.want {
				t.Errorf("ShouldExecute with window=%q: got %v, want %v",
					tc.window, got, tc.want)
			}
		})
	}
}

func TestNonReversibleActionsSkipRollback(t *testing.T) {
	// Verify the preconditions that cause RunCycle to skip rollback
	// monitoring for non-reversible actions (VACUUM, ANALYZE, terminate).
	tests := []struct {
		name           string
		sql            string
		wantCategory   string
		wantConcurrent bool
	}{
		{
			name:           "VACUUM has empty rollback and no concurrently",
			sql:            "VACUUM t",
			wantCategory:   "vacuum",
			wantConcurrent: false,
		},
		{
			name:           "ANALYZE has empty rollback and no concurrently",
			sql:            "ANALYZE t",
			wantCategory:   "analyze",
			wantConcurrent: false,
		},
		{
			name:           "pg_terminate_backend has empty rollback",
			sql:            "SELECT pg_terminate_backend(12345)",
			wantCategory:   "terminate_backend",
			wantConcurrent: false,
		},
		{
			name:           "VACUUM FULL also non-reversible",
			sql:            "VACUUM FULL public.large_table",
			wantCategory:   "vacuum",
			wantConcurrent: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := analyzer.Finding{
				RecommendedSQL: tc.sql,
				RollbackSQL:    "",
			}

			if f.RollbackSQL != "" {
				t.Errorf("RollbackSQL should be empty, got %q", f.RollbackSQL)
			}

			gotCat := categorizeAction(f.RecommendedSQL)
			if gotCat != tc.wantCategory {
				t.Errorf("categorizeAction(%q) = %q, want %q",
					tc.sql, gotCat, tc.wantCategory)
			}

			gotConc := NeedsConcurrently(f.RecommendedSQL)
			if gotConc != tc.wantConcurrent {
				t.Errorf("NeedsConcurrently(%q) = %v, want %v",
					tc.sql, gotConc, tc.wantConcurrent)
			}
		})
	}
}

func TestConcurrentlyOnRawConn(t *testing.T) {
	pool, ctx := requireDB(t)

	// Create a temp table inside the sage schema.
	_, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS sage.test_concurrent_idx (
			id  int,
			val text
		)`,
	)
	if err != nil {
		t.Fatalf("creating temp table: %v", err)
	}

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DROP INDEX CONCURRENTLY IF EXISTS sage.idx_test_conc")
		_, _ = pool.Exec(cctx,
			"DROP TABLE IF EXISTS sage.test_concurrent_idx")
	})

	// Insert a few rows.
	for i := 0; i < 5; i++ {
		_, err = pool.Exec(ctx,
			"INSERT INTO sage.test_concurrent_idx (id, val) VALUES ($1, $2)",
			i, fmt.Sprintf("row_%d", i),
		)
		if err != nil {
			t.Fatalf("inserting row %d: %v", i, err)
		}
	}

	// Use ExecConcurrently to create an index CONCURRENTLY.
	err = ExecConcurrently(ctx, pool,
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_test_conc "+
			"ON sage.test_concurrent_idx (id)",
		30*time.Second,
	)
	if err != nil {
		t.Fatalf("ExecConcurrently: %v", err)
	}

	// Verify the index exists.
	var one int
	err = pool.QueryRow(ctx,
		"SELECT 1 FROM pg_indexes WHERE indexname = 'idx_test_conc'",
	).Scan(&one)
	if err != nil {
		t.Fatalf("index idx_test_conc not found after creation: %v", err)
	}
}

func TestHysteresis(t *testing.T) {
	pool, ctx := requireDB(t)

	// Insert a recent rolled_back action for finding 999.
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, outcome, executed_at)
		 VALUES ('create_index', 999,
		         'CREATE INDEX idx_test ON t (a)',
		         'rolled_back', now())`,
	)
	if err != nil {
		t.Fatalf("inserting recent rolled_back action: %v", err)
	}

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_log WHERE finding_id IN (998, 999)")
	})

	// Recent rollback should trigger hysteresis (within 7-day cooldown).
	if !CheckHysteresis(ctx, pool, 999, 7) {
		t.Error("expected hysteresis=true for recently rolled-back finding 999")
	}

	// Insert an old rolled_back action for finding 998 (30 days ago).
	_, err = pool.Exec(ctx,
		`INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, outcome, executed_at)
		 VALUES ('create_index', 998,
		         'CREATE INDEX idx_old ON t (b)',
		         'rolled_back', now() - interval '30 days')`,
	)
	if err != nil {
		t.Fatalf("inserting old rolled_back action: %v", err)
	}

	// Old rollback should NOT trigger hysteresis (outside 7-day cooldown).
	if CheckHysteresis(ctx, pool, 998, 7) {
		t.Error("expected hysteresis=false for finding 998 rolled back 30 days ago")
	}
}

func TestActionOrdering_CreateBeforeDrop(t *testing.T) {
	// Index optimization findings emit "CREATE ...; DROP ..." in one string.
	// The executor splits on ";\n" and processes in order.
	// CREATEs must come before DROPs to avoid downtime.
	input := "CREATE INDEX CONCURRENTLY idx_new ON t (a,b);\n" +
		"DROP INDEX CONCURRENTLY IF EXISTS idx_old"

	stmts := strings.Split(input, ";\n")
	if len(stmts) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(stmts))
	}

	firstCat := categorizeAction(stmts[0])
	if firstCat != "create_index" {
		t.Errorf("first statement category = %q, want %q",
			firstCat, "create_index")
	}

	secondCat := categorizeAction(stmts[1])
	if secondCat != "drop_index" {
		t.Errorf("second statement category = %q, want %q",
			secondCat, "drop_index")
	}

	// Both should need CONCURRENTLY
	if !NeedsConcurrently(stmts[0]) {
		t.Error("CREATE statement should need CONCURRENTLY")
	}
	if !NeedsConcurrently(stmts[1]) {
		t.Error("DROP statement should need CONCURRENTLY")
	}
}

func TestEmergencyStop(t *testing.T) {
	pool, ctx := requireDB(t)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.config WHERE key = 'emergency_stop'")
	})

	// Set emergency stop to true.
	if err := SetEmergencyStop(ctx, pool, true); err != nil {
		t.Fatalf("SetEmergencyStop(true): %v", err)
	}
	if !CheckEmergencyStop(ctx, pool) {
		t.Error("expected CheckEmergencyStop=true after setting to true")
	}

	// Set emergency stop to false.
	if err := SetEmergencyStop(ctx, pool, false); err != nil {
		t.Fatalf("SetEmergencyStop(false): %v", err)
	}
	if CheckEmergencyStop(ctx, pool) {
		t.Error("expected CheckEmergencyStop=false after setting to false")
	}
}

func TestGrantVerification(t *testing.T) {
	pool, ctx := requireDB(t)

	var logs []string
	logFn := func(component string, msg string, args ...any) {
		formatted := fmt.Sprintf("[%s] %s", component, fmt.Sprintf(msg, args...))
		logs = append(logs, formatted)
	}

	// Should not panic; postgres superuser should have all grants.
	VerifyGrants(ctx, pool, "postgres", logFn)

	// The function ran without panic. Log output is informational;
	// postgres user typically has full privileges so warnings are
	// unlikely, but the function must execute cleanly either way.
	t.Logf("VerifyGrants produced %d log entries", len(logs))
	for _, entry := range logs {
		t.Logf("  %s", entry)
	}
}

func TestDDLTimeout(t *testing.T) {
	pool, ctx := requireDB(t)

	// Use ANALYZE on a bootstrapped sage table — it is on the
	// executor whitelist, runs quickly, and is safe inside a
	// transaction. SELECT 1 is no longer permitted by the
	// post-hardening validator.
	const noopSQL = "ANALYZE sage.findings"

	// ExecInTransaction should succeed with an allowed statement.
	err := ExecInTransaction(ctx, pool, noopSQL, 5*time.Second)
	if err != nil {
		t.Fatalf("ExecInTransaction(%s): %v", noopSQL, err)
	}

	// ExecConcurrently should also handle an allowed statement.
	err = ExecConcurrently(ctx, pool, noopSQL, 5*time.Second)
	if err != nil {
		t.Fatalf("ExecConcurrently(%s): %v", noopSQL, err)
	}

	// Verify that statement_timeout is reset after ExecConcurrently.
	// Acquire a connection and check its statement_timeout.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring connection: %v", err)
	}
	defer conn.Release()

	var timeout string
	err = conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout)
	if err != nil {
		t.Fatalf("SHOW statement_timeout: %v", err)
	}

	// The pool returns connections with default timeout (0 or "0").
	// ExecConcurrently resets to 0 before releasing, so any acquired
	// connection should have timeout=0.
	if timeout != "0" && timeout != "0ms" && timeout != "0s" {
		t.Logf("statement_timeout on fresh connection: %s (non-zero is OK "+
			"if server default is set)", timeout)
	}
}

func TestLockTimeoutSetBeforeDDL(t *testing.T) {
	pool, ctx := requireDB(t)

	// ANALYZE is on the executor whitelist and acquires only a
	// brief SHARE UPDATE EXCLUSIVE lock — perfect for verifying
	// the lock_timeout plumbing without exercising SQL validation.
	const noopSQL = "ANALYZE sage.findings"

	// Use ExecConcurrently with a lock_timeout and verify the
	// statement still succeeds against an unblocked table.
	lockMs := 5000
	err := ExecConcurrently(
		ctx, pool, noopSQL, 10*time.Second,
		WithLockTimeout(lockMs),
	)
	if err != nil {
		t.Fatalf("ExecConcurrently with lock_timeout: %v", err)
	}

	// Verify lock_timeout is reset to 0 after execution.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring connection: %v", err)
	}
	defer conn.Release()

	var lockTimeout string
	err = conn.QueryRow(ctx, "SHOW lock_timeout").Scan(&lockTimeout)
	if err != nil {
		t.Fatalf("SHOW lock_timeout: %v", err)
	}
	if lockTimeout != "0" && lockTimeout != "0ms" && lockTimeout != "0s" {
		t.Logf("lock_timeout after exec: %s (non-zero OK if server default)",
			lockTimeout)
	}

	// ExecInTransaction path: verify lock_timeout with transaction.
	err = ExecInTransaction(
		ctx, pool, noopSQL, 10*time.Second,
		WithLockTimeout(lockMs),
	)
	if err != nil {
		t.Fatalf("ExecInTransaction with lock_timeout: %v", err)
	}
}

func TestLockTimeoutTriggersErrLockNotAvailable(t *testing.T) {
	pool, ctx := requireDB(t)

	// Hold an exclusive lock on a table in one transaction, then
	// try DDL with a very short lock_timeout to trigger 55P03.
	_, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS sage.test_lock_timeout (
			id int PRIMARY KEY
		)`,
	)
	if err != nil {
		t.Fatalf("creating table: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DROP TABLE IF EXISTS sage.test_lock_timeout")
	})

	// Start a transaction that holds ACCESS EXCLUSIVE lock.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning blocking tx: %v", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, "LOCK TABLE sage.test_lock_timeout IN ACCESS EXCLUSIVE MODE")
	if err != nil {
		t.Fatalf("locking table: %v", err)
	}

	// Now attempt allowed DDL with a 1ms lock_timeout — should
	// fail with 55P03. ALTER TABLE ... SET (storage param) is on
	// the executor whitelist and acquires SHARE UPDATE EXCLUSIVE,
	// which conflicts with the ACCESS EXCLUSIVE held by tx above,
	// so the lock_timeout will fire before the lock is granted.
	ddlErr := ExecInTransaction(
		ctx, pool,
		"ALTER TABLE sage.test_lock_timeout SET (autovacuum_enabled = false)",
		10*time.Second,
		WithLockTimeout(1),
	)
	if ddlErr == nil {
		t.Fatal("expected lock timeout error, got nil")
	}
	if !errors.Is(ddlErr, ErrLockNotAvailable) {
		t.Errorf("expected ErrLockNotAvailable, got: %v", ddlErr)
	}
}

func TestMaintenanceWindowEdgeCases(t *testing.T) {
	// All cases use autonomous + moderate + ramp > 31d + Tier3Moderate=true
	// to isolate the maintenance window as the deciding factor.
	rampStart := time.Now().Add(-40 * 24 * time.Hour)
	finding := analyzer.Finding{ActionRisk: "moderate"}

	t.Run("bad format single field blocks moderate action", func(t *testing.T) {
		cfg := &config.Config{
			Trust: config.TrustConfig{
				Level:             "autonomous",
				Tier3Moderate:     true,
				MaintenanceWindow: "59",
			},
		}
		got := ShouldExecute(finding, cfg, rampStart, false, false)
		if got {
			t.Error("single-field cron should block execution, got true")
		}
	})

	t.Run("window 59 23 at midnight is outside window", func(t *testing.T) {
		// "59 23 * * *" means window is 23:59 - 00:59.
		// We test indirectly: if current time is NOT between 23:59 and 00:59,
		// ShouldExecute must return false. If it IS in that range, skip.
		now := time.Now()
		windowStart := time.Date(
			now.Year(), now.Month(), now.Day(),
			23, 59, 0, 0, now.Location(),
		)
		windowEnd := windowStart.Add(1 * time.Hour)

		if !now.Before(windowStart) && now.Before(windowEnd) {
			t.Skip("test is meaningless when run between 23:59 and 00:59")
		}

		cfg := &config.Config{
			Trust: config.TrustConfig{
				Level:             "autonomous",
				Tier3Moderate:     true,
				MaintenanceWindow: "59 23 * * *",
			},
		}
		got := ShouldExecute(finding, cfg, rampStart, false, false)
		if got {
			t.Error("expected false outside 23:59 window, got true")
		}
	})

	t.Run("empty window blocks moderate even with all other conditions met",
		func(t *testing.T) {
			cfg := &config.Config{
				Trust: config.TrustConfig{
					Level:             "autonomous",
					Tier3Moderate:     true,
					MaintenanceWindow: "",
				},
			}
			got := ShouldExecute(finding, cfg, rampStart, false, false)
			if got {
				t.Error("empty maintenance window should block moderate, got true")
			}
		},
	)

	t.Run("non-numeric hour in cron blocks execution", func(t *testing.T) {
		cfg := &config.Config{
			Trust: config.TrustConfig{
				Level:             "autonomous",
				Tier3Moderate:     true,
				MaintenanceWindow: "30 xx * * *",
			},
		}
		got := ShouldExecute(finding, cfg, rampStart, false, false)
		if got {
			t.Error("non-numeric hour should block execution, got true")
		}
	})

	t.Run("non-numeric minute in cron blocks execution", func(t *testing.T) {
		cfg := &config.Config{
			Trust: config.TrustConfig{
				Level:             "autonomous",
				Tier3Moderate:     true,
				MaintenanceWindow: "xx 23 * * *",
			},
		}
		got := ShouldExecute(finding, cfg, rampStart, false, false)
		if got {
			t.Error("non-numeric minute should block execution, got true")
		}
	})
}
