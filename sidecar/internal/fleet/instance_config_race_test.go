package fleet

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// env-race: DatabaseInstance.Config is swapped in place by UpdateMetadata
// (SIGHUP reload) and UpdateInstanceMetadata (dashboard edit). Readers that
// do not hold the manager lock must go through ConfigSnapshot. The tests
// named TestRace_* are written for CI's -race leg; without the detector
// they still pin the values every reader may observe.
//
// Happy path, invalid input and boundary categories do not apply beyond
// the snapshot tests below: the fix adds no new input surface.

func boolPtr(v bool) *bool { return &v }

func TestDatabaseInstanceConfigSnapshot_CopiesReferenceFields(t *testing.T) {
	inst := &DatabaseInstance{Name: "orders", Config: config.DatabaseConfig{
		Name: "orders", Port: 5433, TrustLevel: "advisory",
		Tags:          []string{"tenant=acme"},
		DisabledRules: []string{"seq_scan"},
		Replicas:      []config.DatabaseReplica{{Name: "r1", DSNEnv: "R1_DSN"}},
	}}
	got := inst.ConfigSnapshot()
	if got.Name != "orders" || got.Port != 5433 || got.TrustLevel != "advisory" {
		t.Fatalf("snapshot = %+v, want orders:5433 advisory", got)
	}
	if !reflect.DeepEqual(got.Tags, []string{"tenant=acme"}) ||
		!reflect.DeepEqual(got.DisabledRules, []string{"seq_scan"}) ||
		!reflect.DeepEqual(got.Replicas,
			[]config.DatabaseReplica{{Name: "r1", DSNEnv: "R1_DSN"}}) {
		t.Fatalf("snapshot slices = %+v", got)
	}
	got.Tags[0] = "tenant=globex"
	got.DisabledRules[0] = "none"
	got.Replicas[0].Name = "r9"
	if inst.Config.Tags[0] != "tenant=acme" ||
		inst.Config.DisabledRules[0] != "seq_scan" ||
		inst.Config.Replicas[0].Name != "r1" {
		t.Fatalf("snapshot shares backing arrays with the instance: %+v", inst.Config)
	}
}

func TestDatabaseInstanceConfigSnapshot_PreservesNilAndEmpty(t *testing.T) {
	inst := &DatabaseInstance{Config: config.DatabaseConfig{
		Name: "orders", DisabledRules: []string{},
	}}
	got := inst.ConfigSnapshot()
	if got.Tags != nil || got.Replicas != nil {
		t.Fatalf("nil slices became %#v / %#v", got.Tags, got.Replicas)
	}
	// [] re-enables every rule; nil falls back to the global list.
	if got.DisabledRules == nil || len(got.DisabledRules) != 0 {
		t.Fatalf("empty DisabledRules = %#v, want a non-nil empty list",
			got.DisabledRules)
	}
}

func TestDatabaseInstanceConfigSnapshot_NilInstance(t *testing.T) {
	var inst *DatabaseInstance
	if got := inst.ConfigSnapshot(); !reflect.DeepEqual(got, config.DatabaseConfig{}) {
		t.Fatalf("nil instance snapshot = %+v, want zero config", got)
	}
}

func updateTrustExplicit(
	t *testing.T, mgr *DatabaseManager, inst *DatabaseInstance, explicit bool,
) {
	t.Helper()
	err := mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
		return op.UpdateMetadata(inst, config.DatabaseConfig{
			Name: inst.Name, TrustLevelExplicit: explicit,
			Tags: []string{map[bool]string{true: "x=1", false: "x=0"}[explicit]},
		})
	})
	if err != nil {
		t.Errorf("UpdateMetadata(explicit=%v): %v", explicit, err)
	}
}

// TestRace_DatabaseInstanceConfig_HasTrustLevelOverride: a SIGHUP reload
// swaps Config while the trust owner asks HasTrustLevelOverride, which read
// Config.TrustLevelExplicit without the manager lock.
func TestRace_DatabaseInstanceConfig_HasTrustLevelOverride(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := &DatabaseInstance{Name: "orders", Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders"}}
	mgr.RegisterInstance(inst)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			updateTrustExplicit(t, mgr, inst, i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = inst.HasTrustLevelOverride()
			snap := inst.ConfigSnapshot()
			want := map[bool]string{true: "x=1", false: "x=0"}[snap.TrustLevelExplicit]
			if len(snap.Tags) > 0 && snap.Tags[0] != want {
				t.Errorf("torn snapshot: explicit=%v tags=%v",
					snap.TrustLevelExplicit, snap.Tags)
				return
			}
		}
	}()
	wg.Wait()
	// The last write (i=199) set explicit=false.
	if inst.HasTrustLevelOverride() {
		t.Fatal("HasTrustLevelOverride = true after the last reload set it false")
	}
	updateTrustExplicit(t, mgr, inst, true)
	if !inst.HasTrustLevelOverride() {
		t.Fatal("HasTrustLevelOverride = false after a reload set it true")
	}
	// A runtime-recorded override wins over the YAML bit.
	inst.SetTrustLevelOverride(false)
	if inst.HasTrustLevelOverride() {
		t.Fatal("runtime override false did not win over Config.TrustLevelExplicit")
	}
}
