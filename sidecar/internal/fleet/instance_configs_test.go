package fleet

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// fleet-01: InstanceConfigs is the locked read of per-database configs that
// UpdateMetadata swaps in place.

func TestInstanceConfigsCopiesUnderLock(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&DatabaseInstance{Name: "orders", Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", Port: 5433,
			Tags: []string{"tenant=acme", "tier=prod"}}})
	mgr.RegisterInstance(&DatabaseInstance{Name: "billing", Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "billing"}})
	got := mgr.InstanceConfigs()
	if len(got) != 2 {
		t.Fatalf("configs = %+v, want orders and billing", got)
	}
	if !reflect.DeepEqual(got["orders"].Tags, []string{"tenant=acme", "tier=prod"}) ||
		got["orders"].Port != 5433 || got["billing"].Name != "billing" {
		t.Fatalf("configs = %+v", got)
	}
	if got["billing"].Tags != nil {
		t.Fatalf("billing tags = %#v, want nil", got["billing"].Tags)
	}
	got["orders"].Tags[0] = "tenant=globex"
	if mgr.GetInstance("orders").Config.Tags[0] != "tenant=acme" {
		t.Fatal("InstanceConfigs shares the Tags backing array with the instance")
	}
}

func TestInstanceConfigsEmptyManager(t *testing.T) {
	got := NewManager(&config.Config{Mode: "fleet"}).InstanceConfigs()
	if got == nil || len(got) != 0 {
		t.Fatalf("empty manager configs = %#v, want an empty map", got)
	}
}

func TestFleetStatusTagsAreCopies(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&DatabaseInstance{Name: "orders", Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", Tags: []string{"tenant=acme"}}})
	overview := mgr.FleetStatus()
	if len(overview.Databases) != 1 ||
		!reflect.DeepEqual(overview.Databases[0].Tags, []string{"tenant=acme"}) {
		t.Fatalf("overview = %+v, want orders with tenant=acme", overview.Databases)
	}
	overview.Databases[0].Tags[0] = "tenant=globex"
	if mgr.GetInstance("orders").Config.Tags[0] != "tenant=acme" {
		t.Fatal("FleetStatus shares the Tags backing array with the instance")
	}
}

// TestInstanceConfigsRaceWithUpdateMetadata: CI runs this under -race.
func TestInstanceConfigsRaceWithUpdateMetadata(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := &DatabaseInstance{Name: "orders", Status: &InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", Tags: []string{"tenant=acme"}}}
	mgr.RegisterInstance(inst)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			tag := map[bool]string{true: "tenant=acme", false: "tenant=globex"}[i%2 == 0]
			err := mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
				return op.UpdateMetadata(inst, config.DatabaseConfig{Name: "orders",
					Tags: []string{tag}})
			})
			if err != nil {
				t.Errorf("update %d: %v", i, err)
				return
			}
		}
	}()
	seen := map[string]bool{}
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			for _, c := range mgr.InstanceConfigs() {
				seen[c.Tags[0]] = true
			}
			_ = mgr.FleetStatus()
		}
	}()
	wg.Wait()
	for tag := range seen {
		if tag != "tenant=acme" && tag != "tenant=globex" {
			t.Fatalf("observed torn tag %q", tag)
		}
	}
}
