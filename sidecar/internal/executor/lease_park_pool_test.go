package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The PR #52 CI failure, reproduced: the winner holds its lease connection
// and an execution connection behind the locked table and the blocker holds
// one, so on a small pool (CI's default is 4, shared with the test's own
// queries; 3 here makes it deterministic) the second executor cannot even
// get a connection to see the conflict. It must park within the bounded
// lease wait instead of stalling until the winner's 30s lock timeout fails.
func TestLeaseConflictParksWithSmallPool(t *testing.T) {
	shared, ctx := requireDB(t)
	setupParkTable(t, ctx, shared)
	cfg := shared.Config().Copy()
	cfg.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("small pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var logMu sync.Mutex
	var logs []string
	exec := New(pool, config.DefaultConfig(), time.Time{},
		func(_, format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		})
	exec.emergencyStopFn = func(context.Context) bool { return false }
	first, second := insertParkDecision(t, ctx, shared), insertParkDecision(t, ctx, shared)

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(ctx,
		`LOCK TABLE public.`+parkTable+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock table: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		exec.executeFinding(ctx, parkFinding(), 0, ActionPolicyDecision{DecisionID: first})
	}()
	waitForActiveLease(t, ctx, shared, first)

	waitForLockWaiter(t, ctx, shared)

	// Bound the second call so the pre-fix behaviour fails instead of hanging.
	secondCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	started := time.Now()
	exec.executeFinding(secondCtx, parkFinding(), 0, ActionPolicyDecision{DecisionID: second})
	waited := time.Since(started)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	wg.Wait()
	// Lease wait plus park-record wait, each bounded by LeaseConnectionWait.
	if bound := 2*policy.LeaseConnectionWait + 3*time.Second; waited > bound {
		t.Fatalf("second executor waited %s before parking, want at most %s", waited, bound)
	}
	if !loggedParked(&logMu, logs) {
		t.Fatalf("second executor did not park; logs: %v", logs)
	}
	if rows := actionRows(t, ctx, shared, second); len(rows) != 0 {
		t.Fatalf("parked action wrote action_log rows %v, want none", rows)
	}
	if rows := actionRows(t, ctx, shared, first); len(rows) != 1 || rows[0] == "failed" {
		t.Fatalf("winning action rows = %v, want one non-failed row", rows)
	}
}

// waitForLockWaiter returns once the winner's statement is queued behind the
// blocker's table lock, i.e. it holds its execution connection.
func waitForLockWaiter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%' || $1 || '%')`,
			parkTable).Scan(&waiting)
		if err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("winner never queued behind the table lock")
}

func loggedParked(mu *sync.Mutex, logs []string) bool {
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "parked") && strings.Contains(line, "lease") {
			return true
		}
	}
	return false
}
