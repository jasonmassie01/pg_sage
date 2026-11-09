package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// observability-07 review: a deleted or renamed meta database's runtime
// drains after the store commit, and its in-flight work can still count
// (a shadow decision, a park, a record failure). The prune must run after
// that drain, or the late count leaves a series that no later removal
// clears.

// No concurrent access tests beyond the drain itself: the counters' locking
// is covered by the executor and shadow packages.

func TestMetaDeletePrunesCountsRecordedWhileDraining(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	input.Name = "drain-prune-delete"
	rec, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err != nil || rec == nil {
		t.Fatalf("create: %v", err)
	}
	late := holdDrainForLateCount(t, ctx, input.Name)
	if err := applyMetaDatabaseDelete(ctx, fleetMgr, state, rec.ID, *rec); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertLateCountPruned(t, late, input.Name)
}

func TestMetaRenamePrunesCountsRecordedWhileDraining(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	input.Name = "drain-prune-old"
	rec, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err != nil || rec == nil {
		t.Fatalf("create: %v", err)
	}
	late := holdDrainForLateCount(t, ctx, input.Name)
	input.Name = "drain-prune-new"
	t.Cleanup(func() { shadow.Prune(input.Name) })
	if _, err := applyMetaDatabaseUpdate(ctx, fleetMgr, state, rec.ID, *rec,
		input); err != nil {
		t.Fatalf("rename: %v", err)
	}
	assertLateCountPruned(t, late, rec.Name)
}

// holdDrainForLateCount counts one shadow decision for name now, and
// registers one more worker on its runtime that counts a second decision
// once the first is pruned (or after 3s), then lets the drain finish. The
// returned channel carries that late record's error.
func holdDrainForLateCount(t *testing.T, ctx context.Context, name string) <-chan error {
	t.Helper()
	t.Cleanup(func() { shadow.Prune(name) })
	store := shadow.NewStore(openComposedPool(t, ctx,
		testdb.CreateDatabase(t, "drain_prune")))
	if err := recordContractDecision(ctx, store, name, "public.early"); err != nil {
		t.Fatal(err)
	}
	if !shadowCounted(name) {
		t.Fatalf("fixture: no shadow count for %s", name)
	}
	inst := fleetMgr.GetInstance(name)
	if inst == nil || inst.Workers == nil {
		t.Fatalf("fixture: %s has no runtime workers", name)
	}
	late := make(chan error, 1)
	inst.Workers.Add(1)
	go func() {
		defer inst.Workers.Done()
		deadline := time.Now().Add(3 * time.Second)
		for shadowCounted(name) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		late <- recordContractDecision(ctx, store, name, "public.late")
	}()
	return late
}

func assertLateCountPruned(t *testing.T, late <-chan error, name string) {
	t.Helper()
	select {
	case err := <-late:
		if err != nil {
			t.Fatalf("late record: %v", err)
		}
	default:
		t.Fatal("the removal returned before its runtime's workers finished")
	}
	if shadowCounted(name) {
		t.Errorf("a count recorded while %s drained survived the removal: %v",
			name, shadow.DecisionCounts())
	}
}

func recordContractDecision(
	ctx context.Context, store *shadow.Store, db, object string,
) error {
	sql := "CREATE INDEX CONCURRENTLY ON " + object + " (customer_id)"
	family, class := shadow.ClassOf("create_index_concurrently", sql)
	shape := shadow.Shape(sql)
	d := shadow.Decision{Database: db, Fingerprint: shadow.Fingerprint(class, db, shape),
		Family: family, Class: class, Title: "contract", Object: object, SQL: sql,
		Shape: shape, Evidence: map[string]any{}, GateVerdict: "observe_only",
		GateReason: "autonomy_level", TrustedVerdict: "execute",
		TrustedReason: "autonomy_l3", GrantedLevel: 1}
	_, inserted, err := store.Record(ctx, d)
	if err == nil && !inserted {
		err = fmt.Errorf("shadow decision for %s on %s was not inserted", db, object)
	}
	return err
}

func shadowCounted(db string) bool {
	for _, c := range shadow.DecisionCounts() {
		if c.Database == db {
			return true
		}
	}
	return false
}
