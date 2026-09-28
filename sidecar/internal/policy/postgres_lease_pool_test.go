package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// A lease holds a dedicated connection. When the pool is exhausted the
// acquire must give up within a bounded wait and report a busy lease (a
// park, retried next cycle), not block until its caller's deadline. On a
// 4-CPU CI runner the default pool is 4 connections, and an unbounded wait
// stalled the second executor for 30s instead of parking it.
func TestAcquireLeaseBoundsWaitWhenPoolExhausted(t *testing.T) {
	store := newTestStore(t)
	policyRow := bootstrapPolicy(t, store, Scope{})
	decisionID := insertLeaseDecision(t, store, policyRow.Version)
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(small.Close)
	held, err := small.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold the only connection: %v", err)
	}
	defer held.Release()
	objects, err := NormalizeTargetObjects([]string{"public.orders"})
	if err != nil {
		t.Fatalf("normalize targets: %v", err)
	}
	manager := NewPostgresLeaseManager(small, nil, decisionID, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	_, err = manager.AcquireLease(ctx, "executor", objects, "alter table")
	elapsed := time.Since(started)

	if !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("acquire on exhausted pool = %v, want ErrLeaseBusy", err)
	}
	if elapsed > LeaseConnectionWait+2*time.Second {
		t.Fatalf("acquire waited %s, want at most the %s bound", elapsed, LeaseConnectionWait)
	}
}

// A caller that is already cancelled keeps its own error, not ErrLeaseBusy.
func TestAcquireLeaseKeepsCallerCancellation(t *testing.T) {
	store := newTestStore(t)
	policyRow := bootstrapPolicy(t, store, Scope{})
	decisionID := insertLeaseDecision(t, store, policyRow.Version)
	objects, err := NormalizeTargetObjects([]string{"public.orders"})
	if err != nil {
		t.Fatalf("normalize targets: %v", err)
	}
	manager := NewPostgresLeaseManager(store.pool, nil, decisionID, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = manager.AcquireLease(ctx, "executor", objects, "alter table")
	if err == nil || errors.Is(err, ErrLeaseBusy) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire = %v, want context.Canceled", err)
	}
}
