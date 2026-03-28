//go:build integration

package store

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
	return "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
}

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
)

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

		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}

		if err := schema.Bootstrap(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			testPool.Close()
			testPool = nil
			return
		}

		schema.ReleaseAdvisoryLock(ctx, testPool)
	})

	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, context.Background()
}

func TestPropose(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)

	id, err := s.Propose(
		ctx, nil, 1,
		"CREATE INDEX CONCURRENTLY idx_test ON t (c)",
		"DROP INDEX CONCURRENTLY idx_test",
		"safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})
}

func TestListPending(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)

	id, err := s.Propose(
		ctx, nil, 2,
		"ANALYZE public.orders",
		"", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	actions, err := s.ListPending(ctx, nil)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}

	found := false
	for _, a := range actions {
		if a.ID == id {
			found = true
			if a.Status != "pending" {
				t.Errorf("status = %q, want pending", a.Status)
			}
		}
	}
	if !found {
		t.Error("proposed action not found in pending list")
	}
}

func TestApprove(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)

	id, err := s.Propose(
		ctx, nil, 3,
		"VACUUM public.orders", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	action, err := s.Approve(ctx, id, 1)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if action.Status != "approved" {
		t.Errorf("status = %q, want approved", action.Status)
	}
	if action.DecidedBy == nil || *action.DecidedBy != 1 {
		t.Error("decided_by not set correctly")
	}
}

func TestReject(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)

	id, err := s.Propose(
		ctx, nil, 4,
		"DROP INDEX CONCURRENTLY idx_old", "", "moderate",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	err = s.Reject(ctx, id, 1, "Not needed")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}

	action, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if action.Status != "rejected" {
		t.Errorf("status = %q, want rejected", action.Status)
	}
	if action.Reason != "Not needed" {
		t.Errorf("reason = %q, want 'Not needed'", action.Reason)
	}
}

func TestExpireStale(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)

	// Insert an already-expired action.
	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.action_queue
		    (finding_id, proposed_sql, action_risk,
		     expires_at)
		 VALUES (5, 'SELECT 1', 'safe',
		     now() - INTERVAL '1 day')
		 RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("inserting expired action: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	expired, err := s.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if expired < 1 {
		t.Error("expected at least 1 expired action")
	}

	action, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if action.Status != "expired" {
		t.Errorf("status = %q, want expired", action.Status)
	}
}

func TestApproveAlreadyDecided(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)

	id, err := s.Propose(
		ctx, nil, 6,
		"ANALYZE public.users", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	// Reject first.
	err = s.Reject(ctx, id, 1, "nope")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}

	// Try to approve after rejection — should fail.
	_, err = s.Approve(ctx, id, 2)
	if err == nil {
		t.Fatal("expected error approving already-decided action")
	}
}
