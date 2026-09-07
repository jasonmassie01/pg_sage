package startup

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ── DB pool shared by all coverage-boost tests ──────────────────────

func coverageDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("SAGE_TEST_DATABASE_URL")
}

var (
	cbPool     *pgxpool.Pool
	cbPoolOnce sync.Once
	cbPoolErr  error
)

func coverageDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	cbPoolOnce.Do(func() {
		dsn := coverageDSN()
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			cbPoolErr = fmt.Errorf("parse DSN: %w", err)
			return
		}
		poolCfg.MaxConns = 3
		cbPool, cbPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if cbPoolErr != nil {
			return
		}
		if err := cbPool.Ping(ctx); err != nil {
			cbPoolErr = fmt.Errorf("ping: %w", err)
			cbPool.Close()
			cbPool = nil
		}
	})
	if cbPoolErr != nil {
		t.Skipf("database unavailable: %v", cbPoolErr)
	}
	return cbPool, ctx
}

// ── Integration tests that exercise each internal check function ────

func TestCoverage_RunChecks_Success(t *testing.T) {
	pool, ctx := coverageDB(t)

	result, err := RunChecks(ctx, pool)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if result == nil {
		t.Fatal("RunChecks returned nil result")
	}
	if result.PGVersionNum < 140000 {
		t.Errorf("PGVersionNum = %d, want >= 140000", result.PGVersionNum)
	}
	// Log optional capability flags for diagnostics.
	t.Logf("PGVersionNum=%d QueryTextVisible=%v HasWALColumns=%v HasPlanTimeColumns=%v",
		result.PGVersionNum, result.QueryTextVisible,
		result.HasWALColumns, result.HasPlanTimeColumns)
}

func TestCoverage_CheckConnectivity(t *testing.T) {
	pool, ctx := coverageDB(t)

	err := checkConnectivity(ctx, pool)
	if err != nil {
		t.Fatalf("checkConnectivity: %v", err)
	}
}

func TestCoverage_CheckPGVersion_Valid(t *testing.T) {
	pool, ctx := coverageDB(t)

	versionNum, err := checkPGVersion(ctx, pool)
	if err != nil {
		t.Fatalf("checkPGVersion: %v", err)
	}
	if versionNum < 140000 {
		t.Errorf("versionNum = %d, want >= 140000", versionNum)
	}
}

func TestCoverage_CheckExtensionInstalled(t *testing.T) {
	pool, ctx := coverageDB(t)

	err := checkExtensionInstalled(ctx, pool)
	if err != nil {
		t.Fatalf("checkExtensionInstalled: %v", err)
	}
}

func TestCoverage_CheckExtensionReadable(t *testing.T) {
	pool, ctx := coverageDB(t)

	err := checkExtensionReadable(ctx, pool)
	if err != nil {
		t.Fatalf("checkExtensionReadable: %v", err)
	}
}

func TestCoverage_CheckQueryTextVisible(t *testing.T) {
	pool, ctx := coverageDB(t)

	visible, err := checkQueryTextVisible(ctx, pool)
	if err != nil {
		t.Fatalf("checkQueryTextVisible: %v", err)
	}
	// On a real PG with pg_stat_statements, there should be at least
	// one query captured with text visible.
	t.Logf("QueryTextVisible = %v", visible)
}

func TestCoverage_CheckPlanTimeColumns(t *testing.T) {
	pool, ctx := coverageDB(t)

	hasPlan, err := checkPlanTimeColumns(ctx, pool)
	if err != nil {
		t.Fatalf("checkPlanTimeColumns: %v", err)
	}
	t.Logf("HasPlanTimeColumns = %v", hasPlan)
}

func TestCoverage_CheckWALColumns(t *testing.T) {
	pool, ctx := coverageDB(t)

	hasWAL, err := checkWALColumns(ctx, pool)
	if err != nil {
		t.Fatalf("checkWALColumns: %v", err)
	}
	t.Logf("HasWALColumns = %v", hasWAL)
}

// ── Context cancellation tests ──────────────────────────────────────

func TestCoverage_RunChecks_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	_, err := RunChecks(ctx, pool)
	if err == nil {
		t.Fatal("expected error from RunChecks with cancelled context")
	}
	t.Logf("RunChecks cancelled: %v", err)
}

func TestCoverage_CheckConnectivity_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkConnectivity(ctx, pool)
	if err == nil {
		t.Fatal("expected error from checkConnectivity with cancelled ctx")
	}
}

func TestCoverage_CheckPGVersion_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := checkPGVersion(ctx, pool)
	if err == nil {
		t.Fatal("expected error from checkPGVersion with cancelled ctx")
	}
}

func TestCoverage_CheckExtensionInstalled_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkExtensionInstalled(ctx, pool)
	if err == nil {
		t.Fatal("expected error from checkExtensionInstalled with cancelled ctx")
	}
}

func TestCoverage_CheckExtensionReadable_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkExtensionReadable(ctx, pool)
	if err == nil {
		t.Fatal("expected error from checkExtensionReadable with cancelled ctx")
	}
}

func TestCoverage_CheckQueryTextVisible_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// checkQueryTextVisible returns (false, nil) on scan errors,
	// so a cancelled context should cause the query to fail, resulting
	// in visible=false (the error path returns false, nil).
	visible, err := checkQueryTextVisible(ctx, pool)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if visible {
		t.Error("expected visible=false with cancelled context")
	}
}

func TestCoverage_CheckPlanTimeColumns_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	hasPlan, err := checkPlanTimeColumns(ctx, pool)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasPlan {
		t.Error("expected hasPlan=false with cancelled context")
	}
}

func TestCoverage_CheckWALColumns_CancelledContext(t *testing.T) {
	pool, _ := coverageDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	hasWAL, err := checkWALColumns(ctx, pool)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasWAL {
		t.Error("expected hasWAL=false with cancelled context")
	}
}

// ── Timeout tests ───────────────────────────────────────────────────

func TestCoverage_QueryCtx_Timeout(t *testing.T) {
	// Verify queryCtx applies a ~5s deadline.
	parent := context.Background()
	ctx, cancel := queryCtx(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("queryCtx did not set a deadline")
	}
	remaining := time.Until(deadline)
	// Should be close to 5s (allow 4-6s window for test timing).
	if remaining < 4*time.Second || remaining > 6*time.Second {
		t.Errorf("deadline remaining = %v, want ~5s", remaining)
	}
}

func TestCoverage_QueryCtx_InheritsCancellation(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := queryCtx(parent)
	defer cancel()

	parentCancel()

	select {
	case <-ctx.Done():
		// expected — child inherits parent cancellation
	case <-time.After(1 * time.Second):
		t.Fatal("queryCtx child did not inherit parent cancellation")
	}
}

// ── RunChecks result field validation ───────────────────────────────

func TestCoverage_RunChecks_PGVersionNumIsPositive(t *testing.T) {
	pool, ctx := coverageDB(t)

	result, err := RunChecks(ctx, pool)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if result.PGVersionNum <= 0 {
		t.Errorf("PGVersionNum = %d, want > 0", result.PGVersionNum)
	}
}

func TestCoverage_RunChecks_Idempotent(t *testing.T) {
	pool, ctx := coverageDB(t)

	r1, err := RunChecks(ctx, pool)
	if err != nil {
		t.Fatalf("RunChecks (1st): %v", err)
	}
	r2, err := RunChecks(ctx, pool)
	if err != nil {
		t.Fatalf("RunChecks (2nd): %v", err)
	}

	if r1.PGVersionNum != r2.PGVersionNum {
		t.Errorf("PGVersionNum mismatch: %d vs %d",
			r1.PGVersionNum, r2.PGVersionNum)
	}
	if r1.HasWALColumns != r2.HasWALColumns {
		t.Errorf("HasWALColumns mismatch: %v vs %v",
			r1.HasWALColumns, r2.HasWALColumns)
	}
	if r1.HasPlanTimeColumns != r2.HasPlanTimeColumns {
		t.Errorf("HasPlanTimeColumns mismatch: %v vs %v",
			r1.HasPlanTimeColumns, r2.HasPlanTimeColumns)
	}
}
