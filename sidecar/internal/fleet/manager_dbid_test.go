package fleet

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestGetInstanceByDatabaseID_HappyPath(t *testing.T) {
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "prod",
		DatabaseID: 5,
		Config:     config.DatabaseConfig{Name: "prod"},
		Status:     &InstanceStatus{Connected: true, LastSeen: time.Now()},
	})

	inst := mgr.GetInstanceByDatabaseID(5)
	if inst == nil {
		t.Fatal("expected instance for DatabaseID=5, got nil")
	}
	if inst.Name != "prod" {
		t.Errorf("expected name 'prod', got %q", inst.Name)
	}
	if inst.DatabaseID != 5 {
		t.Errorf("expected DatabaseID=5, got %d", inst.DatabaseID)
	}
}

func TestGetInstanceByDatabaseID_NotFound(t *testing.T) {
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "prod",
		DatabaseID: 5,
		Config:     config.DatabaseConfig{Name: "prod"},
		Status:     &InstanceStatus{Connected: true},
	})

	inst := mgr.GetInstanceByDatabaseID(99)
	if inst != nil {
		t.Errorf("expected nil for DatabaseID=99, got instance %q", inst.Name)
	}
}

func TestUpdateInstanceMetadata_RenamesAndUpdatesStatus(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "old-name",
		DatabaseID: 10,
		Config: config.DatabaseConfig{
			Name:     "old-name",
			Host:     "127.0.0.1",
			Port:     5432,
			User:     "postgres",
			Password: "secret",
			Database: "old_db",
		},
		Status: &InstanceStatus{
			TrustLevel:   "advisory",
			DatabaseName: "old-name",
		},
	})

	updatedCfg := config.DatabaseConfig{
		Name:     "new-name",
		Host:     "127.0.0.1",
		Port:     5432,
		User:     "postgres",
		Password: "secret",
		Database: "new_db",
	}
	ok := mgr.UpdateInstanceMetadata(
		"old-name", "new-name", updatedCfg, 12, "observation")
	if !ok {
		t.Fatal("UpdateInstanceMetadata returned false")
	}
	if mgr.GetInstance("old-name") != nil {
		t.Fatal("old instance key still exists")
	}
	inst := mgr.GetInstance("new-name")
	if inst == nil {
		t.Fatal("new instance key not registered")
	}
	if inst.DatabaseID != 12 {
		t.Fatalf("DatabaseID = %d, want 12", inst.DatabaseID)
	}
	if inst.Config.Database != "new_db" {
		t.Fatalf("Config.Database = %q, want new_db",
			inst.Config.Database)
	}
	snap := inst.SnapshotStatus()
	if snap.TrustLevel != "observation" ||
		snap.DatabaseName != "new-name" {
		t.Fatalf("status not updated: %+v", snap)
	}
}

func TestGetInstanceByDatabaseID_EmptyManager(t *testing.T) {
	mgr := NewManager(&config.Config{})

	inst := mgr.GetInstanceByDatabaseID(1)
	if inst != nil {
		t.Errorf("expected nil from empty manager, got instance %q", inst.Name)
	}
}

func TestGetInstanceByDatabaseID_ZeroID_MatchesUnsetInstances(t *testing.T) {
	// Instances without an explicit DatabaseID have the zero value (0).
	// GetInstanceByDatabaseID(0) will match them. This test documents
	// that behavior so callers know to avoid looking up ID 0 unless
	// they intentionally want to find unset instances.
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:   "unset",
		Config: config.DatabaseConfig{Name: "unset"},
		Status: &InstanceStatus{Connected: true},
		// DatabaseID is implicitly 0 (zero value)
	})

	inst := mgr.GetInstanceByDatabaseID(0)
	if inst == nil {
		t.Fatal("expected GetInstanceByDatabaseID(0) to match instance with unset DatabaseID")
	}
	if inst.Name != "unset" {
		t.Errorf("expected name 'unset', got %q", inst.Name)
	}
}

func TestGetInstanceByDatabaseID_ZeroID_NoMatchWhenAllSet(t *testing.T) {
	// When all instances have explicit non-zero DatabaseIDs,
	// looking up ID 0 returns nil.
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "db1",
		DatabaseID: 1,
		Config:     config.DatabaseConfig{Name: "db1"},
		Status:     &InstanceStatus{Connected: true},
	})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "db2",
		DatabaseID: 2,
		Config:     config.DatabaseConfig{Name: "db2"},
		Status:     &InstanceStatus{Connected: true},
	})

	inst := mgr.GetInstanceByDatabaseID(0)
	if inst != nil {
		t.Errorf("expected nil for ID=0 when all IDs are set, got %q", inst.Name)
	}
}

func TestGetInstanceByDatabaseID_MultipleInstances(t *testing.T) {
	mgr := NewManager(&config.Config{})
	names := []string{"alpha", "beta", "gamma"}
	ids := []int{10, 20, 30}

	for i, name := range names {
		mgr.RegisterInstance(&DatabaseInstance{
			Name:       name,
			DatabaseID: ids[i],
			Config:     config.DatabaseConfig{Name: name},
			Status:     &InstanceStatus{Connected: true},
		})
	}

	for i, id := range ids {
		inst := mgr.GetInstanceByDatabaseID(id)
		if inst == nil {
			t.Fatalf("expected instance for DatabaseID=%d, got nil", id)
		}
		if inst.Name != names[i] {
			t.Errorf("DatabaseID=%d: expected name %q, got %q", id, names[i], inst.Name)
		}
		if inst.DatabaseID != id {
			t.Errorf("DatabaseID=%d: got DatabaseID=%d", id, inst.DatabaseID)
		}
	}
}

func TestGetInstanceByDatabaseID_NegativeID(t *testing.T) {
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "db1",
		DatabaseID: 1,
		Config:     config.DatabaseConfig{Name: "db1"},
		Status:     &InstanceStatus{Connected: true},
	})

	inst := mgr.GetInstanceByDatabaseID(-1)
	if inst != nil {
		t.Errorf("expected nil for negative ID, got instance %q", inst.Name)
	}
}

func TestGetInstanceByDatabaseID_ConcurrentAccess(t *testing.T) {
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "concurrent-db",
		DatabaseID: 42,
		Config:     config.DatabaseConfig{Name: "concurrent-db"},
		Status:     &InstanceStatus{Connected: true},
	})

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent reads should not race with each other.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inst := mgr.GetInstanceByDatabaseID(42)
			if inst == nil {
				t.Error("expected non-nil instance from concurrent read")
			}
		}()
	}

	// Also do concurrent reads for non-existent IDs.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inst := mgr.GetInstanceByDatabaseID(999)
			if inst != nil {
				t.Error("expected nil for non-existent ID from concurrent read")
			}
		}()
	}

	wg.Wait()
}

func TestGetInstanceByDatabaseID_AfterRemoval(t *testing.T) {
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "ephemeral",
		DatabaseID: 7,
		Config:     config.DatabaseConfig{Name: "ephemeral"},
		Status:     &InstanceStatus{Connected: true},
	})

	// Verify it's findable before removal.
	if mgr.GetInstanceByDatabaseID(7) == nil {
		t.Fatal("expected instance before removal")
	}

	mgr.RemoveInstance("ephemeral")

	inst := mgr.GetInstanceByDatabaseID(7)
	if inst != nil {
		t.Errorf("expected nil after removal, got instance %q", inst.Name)
	}
}

func TestGetInstanceByDatabaseID_DuplicateIDs(t *testing.T) {
	// If two instances share the same DatabaseID (shouldn't happen in
	// practice, but tests should cover it), GetInstanceByDatabaseID
	// returns one of them (whichever the map iteration finds first).
	mgr := NewManager(&config.Config{})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "dup1",
		DatabaseID: 3,
		Config:     config.DatabaseConfig{Name: "dup1"},
		Status:     &InstanceStatus{Connected: true},
	})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:       "dup2",
		DatabaseID: 3,
		Config:     config.DatabaseConfig{Name: "dup2"},
		Status:     &InstanceStatus{Connected: true},
	})

	inst := mgr.GetInstanceByDatabaseID(3)
	if inst == nil {
		t.Fatal("expected one of the duplicate-ID instances, got nil")
	}
	if inst.Name != "dup1" && inst.Name != "dup2" {
		t.Errorf("expected dup1 or dup2, got %q", inst.Name)
	}
}
