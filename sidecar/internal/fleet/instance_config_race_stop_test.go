package fleet

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// env-race state transitions: an emergency stop or resume concurrent with a
// reload that changes the per-database executor gate, and a dashboard edit
// concurrent with lock-free config readers.

func updateExecutorGate(
	t *testing.T, mgr *DatabaseManager, inst *DatabaseInstance, enabled bool,
) {
	t.Helper()
	err := mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
		return op.UpdateMetadata(inst, config.DatabaseConfig{
			Name: inst.Name, ExecutorEnabled: boolPtr(enabled),
		})
	})
	if err != nil {
		t.Errorf("UpdateMetadata(executor_enabled=%v): %v", enabled, err)
	}
}

// TestRace_DatabaseInstanceConfig_EmergencyStopDuringReload: the stop latch
// holds through a concurrent reload, and Resume restores the gate of the
// config that is current when it runs (fleet-09 semantics).
func TestRace_DatabaseInstanceConfig_EmergencyStopDuringReload(t *testing.T) {
	mgr := newD8Manager("orders")
	mgr.persistStop = func(context.Context, *DatabaseInstance, bool, string) error {
		return nil
	}
	inst := mgr.GetInstance("orders")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			updateExecutorGate(t, mgr, inst, i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := mgr.EmergencyStopStrict("orders", "race-test"); err != nil {
				t.Errorf("stop %d: %v", i, err)
				return
			}
			if inst.Executor.ExecutorEnabled() {
				t.Errorf("stop %d: executor enabled while the stop latch is held", i)
				return
			}
			if _, err := mgr.ResumeStrict("orders", "race-test"); err != nil {
				t.Errorf("resume %d: %v", i, err)
				return
			}
		}
	}()
	wg.Wait()
	if _, err := mgr.EmergencyStopStrict("orders", "race-test"); err != nil {
		t.Fatalf("final stop: %v", err)
	}
	updateExecutorGate(t, mgr, inst, true)
	if inst.Executor.ExecutorEnabled() {
		t.Fatal("a reload that enables the executor released the emergency stop")
	}
	updateExecutorGate(t, mgr, inst, false)
	if _, err := mgr.ResumeStrict("orders", "race-test"); err != nil {
		t.Fatalf("final resume: %v", err)
	}
	if inst.Executor.ExecutorEnabled() {
		t.Fatal("resume enabled the executor although the current config disables it")
	}
}

// TestRace_DatabaseInstanceConfig_UpdateInstanceMetadata: a dashboard edit
// swaps Config while API handlers read it through ConfigSnapshot.
func TestRace_DatabaseInstanceConfig_UpdateInstanceMetadata(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := &DatabaseInstance{Name: "orders", DatabaseID: 7, Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", TrustLevel: "observation"}}
	mgr.RegisterInstance(inst)
	levels := []string{"observation", "advisory"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			level := levels[i%2]
			cfg := config.DatabaseConfig{Name: "orders", TrustLevel: level,
				Tags: []string{level}}
			if !mgr.UpdateInstanceMetadata("orders", "orders", cfg, 7, level) {
				t.Errorf("update %d refused", i)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			snap := mgr.GetInstanceByDatabaseID(7).ConfigSnapshot()
			if len(snap.Tags) > 0 && snap.Tags[0] != snap.TrustLevel {
				t.Errorf("torn snapshot: level=%q tags=%v", snap.TrustLevel, snap.Tags)
				return
			}
		}
	}()
	wg.Wait()
	if got := inst.ConfigSnapshot().TrustLevel; got != "advisory" {
		t.Fatalf("final trust level = %q, want advisory (last write)", got)
	}
	if got := inst.SnapshotStatus().TrustLevel; got != "advisory" {
		t.Fatalf("final status trust level = %q, want advisory", got)
	}
}
