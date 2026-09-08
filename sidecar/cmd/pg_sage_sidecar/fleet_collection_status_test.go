package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestFleetCollectionStatusPreservesUnknownAndIsolation(t *testing.T) {
	one := &fleet.DatabaseInstance{Status: &fleet.InstanceStatus{DatabaseName: "neon"}}
	two := &fleet.DatabaseInstance{Status: &fleet.InstanceStatus{DatabaseName: "supabase"}}
	applyCollectionSnapshot(one, nil)
	if got := one.SnapshotStatus(); got.DatabaseSize != 0 || !got.CollectorLastRun.IsZero() {
		t.Fatalf("missing snapshot fabricated measurements: %+v", got)
	}
	stamp := time.Now().UTC().Add(-time.Minute)
	snap := &collector.Snapshot{CollectedAt: stamp}
	snap.System.DBSizeBytes = 123456
	applyCollectionSnapshot(one, snap)
	got := one.SnapshotStatus()
	if got.DatabaseName != "neon" || got.DatabaseSize != 123456 ||
		!got.CollectorLastRun.Equal(stamp) || !two.SnapshotStatus().CollectorLastRun.IsZero() {
		t.Fatalf("snapshot lost source identity or timestamp: %+v", got)
	}
	applyCollectionSnapshot(one, &collector.Snapshot{})
	applyCollectionSnapshot(one, &collector.Snapshot{CollectedAt: stamp.Add(-time.Second)})
	if !one.SnapshotStatus().CollectorLastRun.Equal(stamp) {
		t.Fatal("zero timestamp replaced a real collection")
	}
}

func TestFleetCollectionStatusVersionFailureStaysUnknown(t *testing.T) {
	state, _, _ := metaLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inst := &fleet.DatabaseInstance{Pool: state.Pool}
	refreshCollectionStatus(ctx, inst)
	if inst.SnapshotStatus().PGVersion != "" {
		t.Fatal("failed version read fabricated a PostgreSQL version")
	}
	refreshCollectionStatus(context.Background(), inst)
	if inst.SnapshotStatus().PGVersion == "" {
		t.Fatal("successful later read failed to recover the version")
	}
}

func TestFleetCollectionStatusConcurrentRefresh(t *testing.T) {
	inst := &fleet.DatabaseInstance{}
	snap := &collector.Snapshot{CollectedAt: time.Now().UTC()}
	snap.System.DBSizeBytes = 42
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			applyCollectionSnapshot(inst, snap)
			_ = inst.SnapshotStatus()
		}()
	}
	workers.Wait()
	if got := inst.SnapshotStatus(); got.DatabaseSize != 42 ||
		!got.CollectorLastRun.Equal(snap.CollectedAt) {
		t.Fatalf("concurrent refresh lost observation: %+v", got)
	}
}

func TestFleetCollectionStatusUsesManagedCollector(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	input.Name = "status-refresh-fixture"
	prepareMetaGlobals(t)
	cfg.Collector.IntervalSeconds = 1
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	record, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := applyMetaDatabaseDelete(
			context.Background(), fleetMgr, state, record.ID, *record,
		); err != nil {
			t.Errorf("remove status fixture registration: %v", err)
		}
	})
	inst := fleetMgr.GetInstance(input.Name)
	var version string
	if err := inst.Pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	for inst.Collector.LatestSnapshot() == nil && ctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	snapshot := inst.Collector.LatestSnapshot()
	if snapshot == nil {
		t.Fatal("managed collector did not produce a real snapshot")
	}
	updateInstanceFindings(ctx, inst)
	got := inst.SnapshotStatus()
	if got.PGVersion != version || got.DatabaseSize != snapshot.System.DBSizeBytes ||
		got.DatabaseSize <= 0 || !got.CollectorLastRun.Equal(snapshot.CollectedAt) {
		t.Fatalf("managed status disagrees with its PostgreSQL collector: %+v", got)
	}
}

func TestFleetCollectionStatusDoesNotUseGlobalPool(t *testing.T) {
	state, _, _ := metaLifecycleFixture(t)
	oldPool := pool
	pool = state.Pool
	t.Cleanup(func() { pool = oldPool })
	inst := &fleet.DatabaseInstance{Status: &fleet.InstanceStatus{DatabaseName: "missing"}}
	updateInstanceFindings(context.Background(), inst)
	got := inst.SnapshotStatus()
	if got.PGVersion != "" || !got.LastSeen.IsZero() || !got.AnalyzerLastRun.IsZero() {
		t.Fatalf("unconnected instance borrowed global database evidence: %+v", got)
	}
}
