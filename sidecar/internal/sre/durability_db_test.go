package sre

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CHECK-16: a metadata outage blocks action handoff and preserves an
// explicit degraded state; it never reads as healthy.

func unreachableStore(t *testing.T) *PostgresStore {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(pool.Close)
	st, err := NewPostgresStore(pool, DefaultLimits())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st
}

func TestStore_OutageIsMetadataUnavailable(t *testing.T) {
	st := unreachableStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scope := Scope{DeploymentID: NewUUID(), DatabaseID: NewUUID()}
	_, _, err := st.Create(ctx, lockStart(scope, "pid 1"))
	if !errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("Create during outage = %v, want ErrMetadataUnavailable", err)
	}
	if _, err := st.EnsureDeployment(ctx); !errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("EnsureDeployment during outage = %v", err)
	}
}

func TestDurability_OutageBlocksHandoffUntilVerified(t *testing.T) {
	d := NewDurability()
	if !d.HandoffAllowed() || d.Status().Degraded {
		t.Fatal("a new guard must start healthy")
	}
	d.Observe(fmt.Errorf("create: %w", ErrMetadataUnavailable))
	st := d.Status()
	if d.HandoffAllowed() || !st.Degraded || st.Reason != "metadata_unavailable" ||
		st.Since.IsZero() {
		t.Fatalf("after an outage: handoff=%v status=%+v", d.HandoffAllowed(), st)
	}
	d.Observe(nil) // one success is not proof: an explicit check is required
	if d.HandoffAllowed() {
		t.Fatal("a later success cleared the degraded state without a check")
	}
	d.Observe(ErrInvalidRequest) // caller error, not an outage
	if st2 := d.Status(); st2.Since != st.Since {
		t.Fatal("a caller error changed the outage record")
	}

	down := unreachableStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Verify(ctx, down); err == nil || d.HandoffAllowed() {
		t.Fatalf("verify against a dead store = %v, handoff=%v", err, d.HandoffAllowed())
	}
	up, _, _ := liveStore(t, DefaultLimits())
	if err := d.Verify(ctx, up); err != nil || !d.HandoffAllowed() ||
		d.Status().Degraded {
		t.Fatalf("verify against a live store = %v, status %+v", err, d.Status())
	}
}

func TestDurability_CallerErrorsDoNotDegrade(t *testing.T) {
	d := NewDurability()
	for _, err := range []error{ErrInvalidRequest, ErrLeaseLost, ErrBudgetExhausted,
		ErrNotFound, context.Canceled} {
		d.Observe(err)
	}
	if !d.HandoffAllowed() {
		t.Fatalf("caller errors degraded the guard: %+v", d.Status())
	}
	var nilGuard *Durability
	if nilGuard.HandoffAllowed() {
		t.Fatal("a missing guard must never allow handoff")
	}
}
