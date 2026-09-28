package ha

import (
	"context"
	"errors"
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

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func noopLog(string, string, ...any) {}

func TestNew_StartsUnknownAndBlocked(t *testing.T) {
	m := New(nil, noopLog)
	if m.Role() != RoleUnknown {
		t.Errorf("new monitor role = %q, want unknown", m.Role())
	}
	if m.MutationsAllowed() || m.InSafeMode() {
		t.Errorf("new monitor: mutations=%v safe=%v",
			m.MutationsAllowed(), m.InSafeMode())
	}
}

func TestCheck_LivePrimary(t *testing.T) {
	pool, ctx := livePool(t)
	logs := &levelLog{}
	m := New(pool, logs.fn)
	if notPrimary := m.Check(ctx); notPrimary {
		t.Fatal("fixture primary reported as not-primary")
	}
	if m.Role() != RolePrimary || !m.MutationsAllowed() || m.InSafeMode() {
		t.Fatalf("live primary: role=%q mutations=%v safe=%v",
			m.Role(), m.MutationsAllowed(), m.InSafeMode())
	}
	if len(logs.entries) != 1 || logs.entries[0] != "INFO ha: initial role detected: primary" {
		t.Errorf("initial detection log = %q", logs.entries)
	}
}

func TestCheck_ClosedPoolFailsClosed(t *testing.T) {
	pool, _ := livePool(t)
	pool.Close()
	m := New(pool, noopLog)
	if notPrimary := m.Check(context.Background()); !notPrimary {
		t.Fatal("probe failure reported as primary")
	}
	if m.Role() != RoleUnknown || m.MutationsAllowed() {
		t.Fatalf("closed pool: role=%q mutations=%v", m.Role(), m.MutationsAllowed())
	}
}

// A confirmed role after an outage that differs from the last known role
// is a flip (the role changed while unobservable).
func TestCheck_RoleChangeAcrossOutageIsFlip(t *testing.T) {
	m, _, logs := newScripted(
		probeResult{inRecovery: false},
		probeResult{err: errors.New("down")},
		probeResult{inRecovery: true},
	)
	for i := 0; i < 3; i++ {
		m.Check(context.Background())
	}
	if m.Role() != RoleReplica || len(m.flips) != 1 {
		t.Fatalf("role=%q flips=%d, want replica/1 (logs %q)", m.Role(), len(m.flips),
			logs.entries)
	}
}

func TestCheck_ConcurrentCallersAndReaders(t *testing.T) {
	pool, ctx := livePool(t)
	m := New(pool, noopLog)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); m.Check(ctx) }()
		go func() { defer wg.Done(); _ = m.MutationsAllowed() }()
		go func() { defer wg.Done(); _ = m.Role() }()
	}
	wg.Wait()
	if m.Role() != RolePrimary {
		t.Fatalf("role after concurrent checks = %q, want primary", m.Role())
	}
	if len(m.flips) != 0 || m.InSafeMode() {
		t.Fatalf("concurrent stable checks recorded flips=%d safe=%v", len(m.flips),
			m.InSafeMode())
	}
}
