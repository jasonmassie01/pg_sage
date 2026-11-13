package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// env-race review: the TestRace_* tests only prove the fix under -race,
// which this machine cannot run. These tests pin, without the detector,
// that every reader and both writers of DatabaseInstance.Config go through
// configMu: each operation must stay blocked while the opposite side of the
// lock is held, and finish once it is released.
//
// Invalid input and boundary categories do not apply: the lock adds no
// input surface. Nil instances are covered by
// TestDatabaseInstanceConfigSnapshot_NilInstance.

const configLockSettle = 50 * time.Millisecond

// assertWaitsForConfigLock runs op while hold() keeps configMu, checks op
// has not finished, then releases and checks it finishes.
func assertWaitsForConfigLock(
	t *testing.T, name string, hold, release func(), op func(),
) {
	t.Helper()
	hold()
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	select {
	case <-done:
		release()
		t.Fatalf("%s finished while configMu was held: it bypasses the lock", name)
	case <-time.After(configLockSettle):
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s still blocked 5s after configMu was released", name)
	}
}

func TestDatabaseInstanceConfigSnapshot_WaitsForWriter(t *testing.T) {
	inst := &DatabaseInstance{Config: config.DatabaseConfig{
		Name: "orders", TrustLevel: "observation"}}
	var got string
	assertWaitsForConfigLock(t, "ConfigSnapshot",
		inst.configMu.Lock,
		func() {
			inst.Config.TrustLevel = "advisory" // the writer's swap
			inst.configMu.Unlock()
		},
		func() { got = inst.ConfigSnapshot().TrustLevel })
	if got != "advisory" {
		t.Fatalf("snapshot after the writer released = %q, want advisory", got)
	}
}

func TestHasTrustLevelOverride_WaitsForConfigWriter(t *testing.T) {
	inst := &DatabaseInstance{Config: config.DatabaseConfig{Name: "orders"}}
	var got bool
	assertWaitsForConfigLock(t, "HasTrustLevelOverride",
		inst.configMu.Lock,
		func() {
			inst.Config.TrustLevelExplicit = true
			inst.configMu.Unlock()
		},
		func() { got = inst.HasTrustLevelOverride() })
	if !got {
		t.Fatal("HasTrustLevelOverride = false, want the YAML bit set by the writer")
	}
}

func TestUpdateMetadata_WaitsForConfigReaders(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := &DatabaseInstance{Name: "orders", Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", TrustLevel: "observation"}}
	mgr.RegisterInstance(inst)
	var err error
	assertWaitsForConfigLock(t, "UpdateMetadata",
		inst.configMu.RLock,
		func() {
			if inst.Config.TrustLevel != "observation" {
				t.Errorf("Config changed under a reader: %q", inst.Config.TrustLevel)
			}
			inst.configMu.RUnlock()
		},
		func() {
			err = mgr.WithLifecycle(context.Background(),
				func(op *LifecycleMutation) error {
					return op.UpdateMetadata(inst, config.DatabaseConfig{
						Name: "orders", TrustLevel: "advisory"})
				})
		})
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if got := inst.ConfigSnapshot().TrustLevel; got != "advisory" {
		t.Fatalf("trust level after UpdateMetadata = %q, want advisory", got)
	}
}

func TestUpdateInstanceMetadata_WaitsForConfigReaders(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := &DatabaseInstance{Name: "orders", DatabaseID: 7, Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", TrustLevel: "observation"}}
	mgr.RegisterInstance(inst)
	var ok bool
	assertWaitsForConfigLock(t, "UpdateInstanceMetadata",
		inst.configMu.RLock,
		func() {
			if inst.Config.TrustLevel != "observation" {
				t.Errorf("Config changed under a reader: %q", inst.Config.TrustLevel)
			}
			inst.configMu.RUnlock()
		},
		func() {
			ok = mgr.UpdateInstanceMetadata("orders", "orders",
				config.DatabaseConfig{Name: "orders", TrustLevel: "advisory"},
				7, "advisory")
		})
	if !ok {
		t.Fatal("UpdateInstanceMetadata refused an existing instance")
	}
	if got := inst.ConfigSnapshot().TrustLevel; got != "advisory" {
		t.Fatalf("trust level after UpdateInstanceMetadata = %q, want advisory", got)
	}
}
