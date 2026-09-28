package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// D1 T7: with a policy ceiling of 3000ms and the 30000ms safety default, an
// in-transaction ALTER TABLE queued behind an ACCESS EXCLUSIVE holder gives
// up with 55P03 in about three seconds instead of thirty.
func TestInTransactionDDLHonorsPolicyLockCeiling(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "d1_lock_ceiling_target"
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.`+table+`;
		CREATE TABLE public.`+table+` (id int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.`+table) }()
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx,
		`LOCK TABLE public.`+table+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock table: %v", err)
	}

	sql := "ALTER TABLE public." + table + " SET (fillfactor = 90)"
	lockMS := ddlLockTimeoutMS(sql, 30000, 3000)
	if lockMS != 3000 {
		t.Fatalf("effective lock timeout = %dms, want the 3000ms policy ceiling", lockMS)
	}
	started := time.Now()
	err = ExecInTransaction(ctx, pool, sql, time.Minute, WithLockTimeout(lockMS))
	elapsed := time.Since(started)

	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("ExecInTransaction error = %v, want ErrLockNotAvailable", err)
	}
	// The server times lock_timeout on its own clock, which on a loaded dev VM
	// can run ahead of the host; the exact ceiling is asserted above. Here,
	// prove the wait was bounded by the 3s ceiling, not the 30s safety value.
	if elapsed > 10*time.Second {
		t.Fatalf("lock wait took %s, want the 3s policy ceiling, not 30s", elapsed)
	}
}

// executeFinding passes the decision's policy ceiling to in-transaction DDL.
func TestExecuteFindingAppliesDecisionLockCeiling(t *testing.T) {
	pool, ctx := requireDB(t)
	setupParkTable(t, ctx, pool)
	exec := New(pool, config.DefaultConfig(), nil, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx,
		`LOCK TABLE public.`+parkTable+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock table: %v", err)
	}

	started := time.Now()
	exec.executeFinding(ctx, parkFinding(), 0, ActionPolicyDecision{LockCeilingMS: 1000})
	elapsed := time.Since(started)

	if elapsed > 5*time.Second {
		t.Fatalf("executeFinding waited %s on the lock; the 1000ms policy ceiling "+
			"was not applied (safety default is 30s)", elapsed)
	}
}
