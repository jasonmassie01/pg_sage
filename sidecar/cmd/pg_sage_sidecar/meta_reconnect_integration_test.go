package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func TestMetaReconnectReplacesFailedGenerationAndHonorsStop(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	id, err := state.Store.Create(ctx, input, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Store.Delete(ctx, id) })
	rec, err := state.Store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	registerFailedInstance(*rec, "fixture disconnected")
	failed := fleetMgr.GetInstance(input.Name)
	failed.Stopped = true
	retryFailedInstances(state)
	if fleetMgr.GetInstance(input.Name) != failed || failed.Pool != nil {
		t.Fatal("manually stopped runtime was reconnected")
	}
	failed.Stopped = false
	retryFailedInstances(state)
	healthy := fleetMgr.GetInstance(input.Name)
	assertManagedRuntime(t, healthy, id, input.Name)
	if healthy == failed || healthy.SnapshotStatus().Error != "" {
		t.Fatal("reconnect preserved failed generation/error")
	}
	retryFailedInstances(state)
	if fleetMgr.GetInstance(input.Name) != healthy {
		t.Fatal("healthy generation was needlessly replaced")
	}
}

func TestMetaStartupRegistersOnlyEnabledDatabases(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	id, err := state.Store.Create(ctx, input, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Store.Delete(ctx, id) })
	input.Name = "disabled-db"
	disabledID, err := state.Store.Create(ctx, input, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Store.Delete(ctx, disabledID) })
	if _, err := state.Pool.Exec(ctx, "UPDATE sage.databases SET enabled=false WHERE id=$1",
		disabledID); err != nil {
		t.Fatal(err)
	}
	cfg.LLM.FleetTokenBudgetDaily = 1000
	initMetaDBFleet(state)
	if fleetMgr.InstanceCount() != 1 || fleetMgr.GetInstance("disabled-db") != nil ||
		fleetLLMBudget == nil || cap(analyzeSem) < 1 {
		t.Fatal("metadata startup did not honor enabled filter/runtime owners")
	}
	assertManagedRuntime(t, fleetMgr.GetInstance("managed-a"), id, "managed-a")
}

func TestMetaRegistrationMissingStoreRecordPublishesOnlyFailure(t *testing.T) {
	state, _, _ := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	registerStoreDatabase(state, store.DatabaseRecord{ID: 999999, Name: "missing"})
	inst := fleetMgr.GetInstance("missing")
	if inst == nil || inst.Pool != nil || inst.Executor != nil || inst.SnapshotStatus().Error == "" {
		t.Fatal("missing credentials did not produce isolated failed placeholder")
	}
	if err := healthCheckStoreDatabase(context.Background(), inst); err != fleet.ErrInvalidInstance {
		t.Fatalf("failed placeholder health=%v", err)
	}
}
