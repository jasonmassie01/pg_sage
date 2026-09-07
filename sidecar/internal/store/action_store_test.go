//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
	findingID := insertActionFinding(t, pool, ctx, "list_pending")

	id, err := s.Propose(
		ctx, nil, findingID,
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
	findingID := insertActionFinding(t, pool, ctx, "approve")

	id, err := s.Propose(
		ctx, nil, findingID,
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

func insertActionFinding(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, suffix string,
) int {
	t.Helper()
	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		    (category, severity, object_type, object_identifier,
		     title, detail, status)
		 VALUES ($1, 'warning', 'table', $2, $3, '{}'::jsonb, 'open')
		 RETURNING id`,
		"action_store_test",
		fmt.Sprintf("action_store_test_%s", suffix),
		fmt.Sprintf("Action store test %s", suffix),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.findings WHERE id = $1", id)
	})
	return id
}
