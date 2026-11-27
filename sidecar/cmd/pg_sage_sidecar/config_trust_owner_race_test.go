package main

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// env-race: currentTrustLevel and HasTrustLevelOverride read the
// per-database config while a SIGHUP reload swaps it (UpdateMetadata).
// Written for CI's -race leg; without the detector it pins that every read
// returns one of the configured levels, and the last write wins.

func TestRace_CurrentTrustLevelDuringReload(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	inst := &fleet.DatabaseInstance{Name: "orders", Status: &fleet.InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", TrustLevel: "observation"}}
	mgr.RegisterInstance(inst)
	levels := []string{"observation", "advisory"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cfg := config.DatabaseConfig{Name: "orders", TrustLevel: levels[i%2],
				TrustLevelExplicit: i%2 == 1}
			err := mgr.WithLifecycle(context.Background(),
				func(op *fleet.LifecycleMutation) error {
					return op.UpdateMetadata(inst, cfg)
				})
			if err != nil {
				t.Errorf("reload %d: %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			got := currentTrustLevel(inst, "autonomous")
			if got != "observation" && got != "advisory" {
				t.Errorf("read %d: trust level %q, want a configured level", i, got)
				return
			}
			_ = inst.HasTrustLevelOverride()
		}
	}()
	wg.Wait()
	if got := currentTrustLevel(inst, "autonomous"); got != "advisory" {
		t.Fatalf("trust level after reloads = %q, want advisory (last write)", got)
	}
	if !inst.HasTrustLevelOverride() {
		t.Fatal("HasTrustLevelOverride = false, want the last reload's explicit level")
	}
}
