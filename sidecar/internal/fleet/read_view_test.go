package fleet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// apiauth-08b: ReadView is the fleet as a database-scoped user may read
// it. Handlers built over the view see only the visible instances; the
// view never changes the fleet it came from.
// No PostgreSQL test here: a view copies pointers and never opens a
// connection (the API's database read tests run it on PostgreSQL).

func viewFleet(t *testing.T) *DatabaseManager {
	t.Helper()
	cfg := &config.Config{Mode: "fleet"}
	mgr := NewManager(cfg)
	for _, name := range []string{"billing", "search", "zeta"} {
		mgr.RegisterInstance(&DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name}, Pool: &pgxpool.Pool{},
			Status: &InstanceStatus{Connected: true, FindingsOpen: 2}})
	}
	return mgr
}

func TestReadView_HoldsOnlyVisibleInstances(t *testing.T) {
	mgr := viewFleet(t)
	v := mgr.ReadView(map[string]bool{"search": true, "removed": true})
	if v.InstanceCount() != 1 || v.GetInstance("search") == nil {
		t.Fatalf("view instances = %v, want only search", v.Instances())
	}
	if v.GetInstance("billing") != nil || v.GetInstance("removed") != nil {
		t.Fatal("the view exposes an instance outside the visible set")
	}
	if v.GetInstance("search") != mgr.GetInstance("search") {
		t.Fatal("the view must share the live instance, not a copy")
	}
	if got := len(v.AllPools()); got != 1 {
		t.Fatalf("AllPools = %d pools, want 1", got)
	}
	if v.PoolForDatabase("billing") != nil {
		t.Fatal("PoolForDatabase answers a database outside the view")
	}
	if v.PoolForDatabase("all") != mgr.GetInstance("search").Pool {
		t.Fatal(`PoolForDatabase("all") must be the visible database's pool`)
	}
	if got := v.ResolveDatabaseName("all"); got != "search" {
		t.Fatalf(`ResolveDatabaseName("all") = %q, want search (one visible)`, got)
	}
	if v.Config() != mgr.Config() {
		t.Fatal("the view must keep the fleet's config")
	}
}

func TestReadView_FleetStatusCountsVisibleOnly(t *testing.T) {
	mgr := viewFleet(t)
	whole := mgr.FleetStatus()
	v := mgr.ReadView(map[string]bool{"billing": true, "zeta": true}).FleetStatus()
	if whole.Summary.TotalDatabases != 3 {
		t.Fatalf("fleet total = %d, want 3", whole.Summary.TotalDatabases)
	}
	if v.Summary.TotalDatabases != 2 || len(v.Databases) != 2 {
		t.Fatalf("view total = %d rows %d, want 2", v.Summary.TotalDatabases,
			len(v.Databases))
	}
	if v.Summary.TotalFindings != 4 {
		t.Fatalf("view findings = %d, want 4 (2 per visible database)",
			v.Summary.TotalFindings)
	}
	for _, db := range v.Databases {
		if db.Name == "search" {
			t.Fatal("FleetStatus of the view lists search")
		}
	}
	if v.Mode != "fleet" {
		t.Fatalf("mode = %q, want fleet", v.Mode)
	}
}

func TestReadView_EmptyAndNilSets(t *testing.T) {
	mgr := viewFleet(t)
	for name, set := range map[string]map[string]bool{"nil": nil, "empty": {},
		"only removed": {"gone": true}} {
		v := mgr.ReadView(set)
		if v.InstanceCount() != 0 || v.PoolForDatabase("all") != nil ||
			len(v.FleetStatus().Databases) != 0 {
			t.Fatalf("%s: view is not empty: %v", name, v.Instances())
		}
		if got := v.ResolveDatabaseName(""); got != "" {
			t.Fatalf("%s: ResolveDatabaseName on an empty view = %q", name, got)
		}
	}
}

func TestReadView_PrimaryFallsBackToVisible(t *testing.T) {
	mgr := viewFleet(t) // primary is billing, the first registered
	if mgr.ResolveDatabaseName("") != "all" {
		t.Fatal("precondition: three databases resolve to all")
	}
	v := mgr.ReadView(map[string]bool{"zeta": true, "search": true})
	if got := v.PoolForDatabase(""); got != mgr.GetInstance("search").Pool {
		t.Fatal("with the primary hidden, the first visible connected pool must answer")
	}
}

func TestReadView_RefusesWrites(t *testing.T) {
	mgr := viewFleet(t)
	v := mgr.ReadView(map[string]bool{"billing": true})
	err := v.WithLifecycle(context.Background(), func(*LifecycleMutation) error {
		t.Fatal("a lifecycle mutation ran on a read view")
		return nil
	})
	if !errors.Is(err, ErrReadOnlyView) {
		t.Fatalf("WithLifecycle on a view = %v, want ErrReadOnlyView", err)
	}
	if _, err := v.EmergencyStopStrict("billing", "tester"); !errors.Is(err, ErrReadOnlyView) {
		t.Fatalf("EmergencyStopStrict on a view = %v, want ErrReadOnlyView", err)
	}
	if mgr.GetInstance("billing").Stopped {
		t.Fatal("a stop through the view reached the fleet")
	}
	v.RegisterInstance(&DatabaseInstance{Name: "new"})
	if mgr.GetInstance("new") != nil {
		t.Fatal("RegisterInstance on a view reached the fleet")
	}
}

func TestReadView_NilManager(t *testing.T) {
	var mgr *DatabaseManager
	if v := mgr.ReadView(map[string]bool{"billing": true}); v != nil {
		t.Fatalf("ReadView of a nil fleet = %v, want nil", v)
	}
}

func TestReadView_ConcurrentWithFleetChanges(t *testing.T) {
	mgr := viewFleet(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("extra%02d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			v := mgr.ReadView(map[string]bool{"billing": true})
			if v.InstanceCount() > 1 {
				t.Errorf("view grew to %d instances", v.InstanceCount())
			}
			_ = v.FleetStatus()
		}()
		go func() {
			defer wg.Done()
			mgr.RegisterInstance(&DatabaseInstance{Name: name,
				Config: config.DatabaseConfig{Name: name}, Pool: &pgxpool.Pool{},
				Status: &InstanceStatus{Connected: true}})
		}()
	}
	wg.Wait()
	if mgr.InstanceCount() != 23 {
		t.Fatalf("fleet has %d instances after the race, want 23", mgr.InstanceCount())
	}
}
