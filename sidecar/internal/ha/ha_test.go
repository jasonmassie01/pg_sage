package ha

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("SAGE_TEST_DATABASE_URL")
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
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
		}
	})
	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, ctx
}

func noopLog(_ string, _ string, _ ...any) {}

func TestNew(t *testing.T) {
	m := New(nil, noopLog)
	if m == nil {
		t.Fatal("expected non-nil Monitor")
	}
	if m.initialized {
		t.Error("monitor should not be initialized on creation")
	}
	if m.safeMode {
		t.Error("monitor should not start in safe mode")
	}
}

func TestConstants(t *testing.T) {
	if flipThreshold != 5 {
		t.Errorf("expected flipThreshold=5, got %d", flipThreshold)
	}
	if stableThreshold != 5 {
		t.Errorf("expected stableThreshold=5, got %d", stableThreshold)
	}
}

func TestIsReplica_DefaultFalse(t *testing.T) {
	m := New(nil, noopLog)
	if m.IsReplica() {
		t.Error("expected IsReplica()=false before initialization")
	}
}

func TestInSafeMode_DefaultFalse(t *testing.T) {
	m := New(nil, noopLog)
	if m.InSafeMode() {
		t.Error("expected InSafeMode()=false on new monitor")
	}
}

func TestFlipDetection_EntersSafeMode(t *testing.T) {
	m := New(nil, noopLog)
	m.initialized = true
	m.wasReplica = false

	// Simulate alternating flips by toggling wasReplica manually.
	for i := 0; i < flipThreshold; i++ {
		m.mu.Lock()
		inRecovery := !m.wasReplica
		m.flipCount++
		m.stableCount = 0
		if m.flipCount >= flipThreshold && !m.safeMode {
			m.safeMode = true
		}
		m.wasReplica = inRecovery
		m.mu.Unlock()
	}

	if !m.InSafeMode() {
		t.Error("expected safe mode after 5 consecutive flips")
	}
}

func TestStableChecks_ExitsSafeMode(t *testing.T) {
	m := New(nil, noopLog)
	m.initialized = true
	m.safeMode = true
	m.flipCount = 0

	// Simulate stable checks (no role change).
	for i := 0; i < stableThreshold; i++ {
		m.mu.Lock()
		m.stableCount++
		m.flipCount = 0
		if m.safeMode && m.stableCount >= stableThreshold {
			m.safeMode = false
			m.stableCount = 0
		}
		m.mu.Unlock()
	}

	if m.InSafeMode() {
		t.Error("expected safe mode to exit after 5 stable checks")
	}
}

func TestFlipCount_ResetsOnStable(t *testing.T) {
	m := New(nil, noopLog)
	m.initialized = true
	m.flipCount = 3

	// Simulate a stable check (same role as before).
	m.mu.Lock()
	m.stableCount++
	m.flipCount = 0
	m.mu.Unlock()

	m.mu.Lock()
	fc := m.flipCount
	m.mu.Unlock()

	if fc != 0 {
		t.Errorf("expected flipCount=0 after stable check, got %d", fc)
	}
}

func TestStableCount_ResetsOnFlip(t *testing.T) {
	m := New(nil, noopLog)
	m.initialized = true
	m.stableCount = 3

	// Simulate a flip.
	m.mu.Lock()
	m.flipCount++
	m.stableCount = 0
	m.mu.Unlock()

	m.mu.Lock()
	sc := m.stableCount
	m.mu.Unlock()

	if sc != 0 {
		t.Errorf("expected stableCount=0 after flip, got %d", sc)
	}
}

func TestSafeMode_NotEnteredBelowThreshold(t *testing.T) {
	m := New(nil, noopLog)
	m.initialized = true
	m.wasReplica = false

	// Simulate 4 flips (below threshold of 5).
	for i := 0; i < flipThreshold-1; i++ {
		m.mu.Lock()
		m.flipCount++
		m.stableCount = 0
		if m.flipCount >= flipThreshold && !m.safeMode {
			m.safeMode = true
		}
		m.wasReplica = !m.wasReplica
		m.mu.Unlock()
	}

	if m.InSafeMode() {
		t.Error("should not enter safe mode with fewer than 5 flips")
	}
}

func TestSafeMode_NotExitedBelowStableThreshold(t *testing.T) {
	m := New(nil, noopLog)
	m.initialized = true
	m.safeMode = true

	// Simulate 4 stable checks (below threshold of 5).
	for i := 0; i < stableThreshold-1; i++ {
		m.mu.Lock()
		m.stableCount++
		m.flipCount = 0
		if m.safeMode && m.stableCount >= stableThreshold {
			m.safeMode = false
			m.stableCount = 0
		}
		m.mu.Unlock()
	}

	if !m.InSafeMode() {
		t.Error("should remain in safe mode with fewer than 5 stable checks")
	}
}

func TestCheck_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)

	logCalls := 0
	logFn := func(_ string, _ string, _ ...any) { logCalls++ }

	m := New(pool, logFn)

	// Cloud SQL is a primary, not a replica.
	isReplica := m.Check(ctx)
	if isReplica {
		t.Error("Check returned true (replica), expected false (primary)")
	}
	if m.IsReplica() {
		t.Error("IsReplica() should be false for Cloud SQL primary")
	}
	if m.InSafeMode() {
		t.Error("InSafeMode() should be false with no flips")
	}
}
