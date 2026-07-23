package policy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPostgresLeaseSerializesOverlappingDDLAndReleasesSession(t *testing.T) {
	store := newTestStore(t)
	policyRow := bootstrapPolicy(t, store, Scope{})
	decisionID := insertLeaseDecision(t, store, policyRow.Version)
	first := NewPostgresLeaseManager(store.pool, nil, decisionID, time.Minute)
	second := NewPostgresLeaseManager(store.pool, nil, decisionID, time.Minute)
	objects, err := NormalizeTargetObjects([]string{"public.orders"})
	if err != nil {
		t.Fatalf("normalize targets: %v", err)
	}

	leaseID, err := first.AcquireLease(
		context.Background(), "executor-a", objects, "create index",
	)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := second.AcquireLease(
		context.Background(), "executor-b", objects, "alter table",
	); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("second acquire error = %v, want ErrLeaseConflict", err)
	}
	if err := first.ReleaseLease(context.Background(), leaseID); err != nil {
		t.Fatalf("release: %v", err)
	}
	secondID, err := second.AcquireLease(
		context.Background(), "executor-b", objects, "alter table",
	)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.ReleaseLease(context.Background(), secondID); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestPostgresLeaseRejectsMissingDecisionAndEmptyTargets(t *testing.T) {
	store := newTestStore(t)
	manager := NewPostgresLeaseManager(store.pool, nil, 0, time.Minute)

	if _, err := manager.AcquireLease(
		context.Background(), "executor", nil, "migration",
	); err == nil {
		t.Fatal("empty targets unexpectedly acquired")
	}
	objects, _ := NormalizeTargetObjects([]string{"public.orders"})
	if _, err := manager.AcquireLease(
		context.Background(), "executor", objects, "migration",
	); err == nil {
		t.Fatal("missing decision unexpectedly acquired")
	}
}

func insertLeaseDecision(t *testing.T, store *Store, version int64) int64 {
	t.Helper()
	var id int64
	err := store.pool.QueryRow(context.Background(), `INSERT INTO sage.decision
		(feature,intent,policy_version,verdict,risk_tier,reason,evidence_id)
		VALUES ('migration','test',$1,'execute','moderate','test',$2)
		RETURNING id`, version, "lease-test-"+t.Name()).Scan(&id)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return id
}
