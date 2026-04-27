package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

func newTestManager(databases ...string) *DatabaseManager {
	cfg := &config.Config{Mode: "fleet"}
	mgr := NewManager(cfg)
	for _, name := range databases {
		mgr.RegisterInstance(&DatabaseInstance{
			Name:   name,
			Config: config.DatabaseConfig{Name: name},
			Status: &InstanceStatus{
				Connected: true,
				LastSeen:  time.Now(),
			},
		})
	}
	return mgr
}

func TestManager_SingleDatabase(t *testing.T) {
	mgr := newTestManager("db1")
	if mgr.InstanceCount() != 1 {
		t.Fatalf("expected 1 instance, got %d", mgr.InstanceCount())
	}
	inst := mgr.GetInstance("db1")
	if inst == nil {
		t.Fatal("expected instance db1")
	}
	if inst.Name != "db1" {
		t.Errorf("expected name db1, got %s", inst.Name)
	}
}

func TestManager_MultipleDatabases(t *testing.T) {
	mgr := newTestManager("db1", "db2", "db3")
	if mgr.InstanceCount() != 3 {
		t.Fatalf("expected 3, got %d", mgr.InstanceCount())
	}
}

func TestManager_GetInstance_NotFound(t *testing.T) {
	mgr := newTestManager("db1")
	if mgr.GetInstance("nope") != nil {
		t.Error("expected nil for unknown instance")
	}
}

func TestManager_FleetStatus_AllHealthy(t *testing.T) {
	mgr := newTestManager("a", "b", "c")
	status := mgr.FleetStatus()
	if status.Summary.TotalDatabases != 3 {
		t.Errorf("total: %d", status.Summary.TotalDatabases)
	}
	if status.Summary.Healthy != 3 {
		t.Errorf("healthy: %d", status.Summary.Healthy)
	}
	if status.Summary.Degraded != 0 {
		t.Errorf("degraded: %d", status.Summary.Degraded)
	}
}

func TestManager_FleetStatus_OneDegraded(t *testing.T) {
	mgr := newTestManager("a", "b", "c")
	mgr.GetInstance("b").Status.Error = "connection refused"
	status := mgr.FleetStatus()
	if status.Summary.Healthy != 2 {
		t.Errorf("healthy: %d", status.Summary.Healthy)
	}
	if status.Summary.Degraded != 1 {
		t.Errorf("degraded: %d", status.Summary.Degraded)
	}
}

func TestManager_FleetStatus_Disconnected(t *testing.T) {
	mgr := newTestManager("a")
	mgr.GetInstance("a").Status.Connected = false
	status := mgr.FleetStatus()
	if status.Summary.Degraded != 1 {
		t.Errorf("degraded: %d", status.Summary.Degraded)
	}
	if status.Databases[0].Status.HealthScore != 0 {
		t.Errorf("health: %d", status.Databases[0].Status.HealthScore)
	}
}

func TestManager_HealthScore_NoFindings(t *testing.T) {
	s := &InstanceStatus{Connected: true}
	if score := computeHealthScore(s); score != 100 {
		t.Errorf("expected 100, got %d", score)
	}
}

func TestManager_HealthScore_CriticalFindings(t *testing.T) {
	s := &InstanceStatus{
		Connected:        true,
		FindingsCritical: 1,
		FindingsWarning:  3,
	}
	// 100 - 25 - 15 = 60
	if score := computeHealthScore(s); score != 60 {
		t.Errorf("expected 60, got %d", score)
	}
}

func TestManager_HealthScore_Floor(t *testing.T) {
	s := &InstanceStatus{Connected: true, FindingsCritical: 5}
	// 100 - 125 = 0 (floor)
	if score := computeHealthScore(s); score != 0 {
		t.Errorf("expected 0, got %d", score)
	}
}

func TestManager_HealthScore_ErrorOverrides(t *testing.T) {
	s := &InstanceStatus{Connected: true, Error: "something"}
	if score := computeHealthScore(s); score != 0 {
		t.Errorf("expected 0, got %d", score)
	}
}

func TestManager_FleetStatus_SortedByHealth(t *testing.T) {
	mgr := newTestManager("good", "bad", "mid")
	mgr.GetInstance("bad").Status.FindingsCritical = 3 // 100-75=25
	mgr.GetInstance("mid").Status.FindingsWarning = 5  // 100-25=75
	status := mgr.FleetStatus()
	if status.Databases[0].Name != "bad" {
		t.Errorf("expected bad first, got %s", status.Databases[0].Name)
	}
}

func TestManager_FleetStatus_FindingsTotals(t *testing.T) {
	mgr := newTestManager("a", "b")
	mgr.GetInstance("a").Status.FindingsOpen = 5
	mgr.GetInstance("a").Status.FindingsCritical = 2
	mgr.GetInstance("b").Status.FindingsOpen = 3
	mgr.GetInstance("b").Status.FindingsCritical = 1
	status := mgr.FleetStatus()
	if status.Summary.TotalFindings != 8 {
		t.Errorf("findings: %d", status.Summary.TotalFindings)
	}
	if status.Summary.TotalCritical != 3 {
		t.Errorf("critical: %d", status.Summary.TotalCritical)
	}
}

func TestManager_EmergencyStop_Single(t *testing.T) {
	mgr := newTestManager("a", "b")
	stopped := mgr.EmergencyStop("a")
	if stopped != 1 {
		t.Errorf("stopped: %d", stopped)
	}
	if !mgr.GetInstance("a").Stopped {
		t.Error("a should be stopped")
	}
	if mgr.GetInstance("b").Stopped {
		t.Error("b should not be stopped")
	}
}

func TestManager_EmergencyStop_All(t *testing.T) {
	mgr := newTestManager("a", "b", "c")
	stopped := mgr.EmergencyStop("")
	if stopped != 3 {
		t.Errorf("stopped: %d", stopped)
	}
}

func TestManager_EmergencyStop_AlreadyStopped(t *testing.T) {
	mgr := newTestManager("a")
	mgr.EmergencyStop("a")
	stopped := mgr.EmergencyStop("a")
	if stopped != 0 {
		t.Errorf("expected 0 (already stopped), got %d", stopped)
	}
}

func TestManager_EmergencyStopStrict_UnknownDatabase(t *testing.T) {
	mgr := newTestManager("a")
	stopped, err := mgr.EmergencyStopStrict("missing")
	if !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("error = %v, want ErrDatabaseNotFound", err)
	}
	if stopped != 0 {
		t.Fatalf("stopped = %d, want 0", stopped)
	}
	if mgr.GetInstance("a").Stopped {
		t.Fatal("known database was stopped after unknown target")
	}
}

func TestManager_EmergencyStopStrict_PersistenceError(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://user:pass@localhost/db")
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	pool.Close()

	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&DatabaseInstance{
		Name:   "a",
		Pool:   pool,
		Config: config.DatabaseConfig{Name: "a"},
		Status: &InstanceStatus{Connected: true},
	})

	stopped, err := mgr.EmergencyStopStrict("a")
	if err == nil {
		t.Fatal("expected persistence error")
	}
	if stopped != 0 {
		t.Fatalf("stopped = %d, want 0", stopped)
	}
	if mgr.GetInstance("a").Stopped {
		t.Fatal("instance marked stopped despite persistence failure")
	}
}

func TestManager_EmergencyStop_DoesNotCancelMonitoring(t *testing.T) {
	mgr := newTestManager("a")
	cancelled := false
	mgr.GetInstance("a").Cancel = func() { cancelled = true }

	stopped := mgr.EmergencyStop("a")
	if stopped != 1 {
		t.Fatalf("stopped: got %d, want 1", stopped)
	}
	if cancelled {
		t.Fatal("EmergencyStop must not cancel monitoring goroutines")
	}
}

func TestManager_Resume(t *testing.T) {
	mgr := newTestManager("a", "b")
	mgr.EmergencyStop("")
	resumed := mgr.Resume("a")
	if resumed != 1 {
		t.Errorf("resumed: %d", resumed)
	}
	if mgr.GetInstance("a").Stopped {
		t.Error("a should be running")
	}
	if !mgr.GetInstance("b").Stopped {
		t.Error("b should still be stopped")
	}
}

func TestManager_Resume_NotStopped(t *testing.T) {
	mgr := newTestManager("a")
	resumed := mgr.Resume("a")
	if resumed != 0 {
		t.Errorf("expected 0, got %d", resumed)
	}
}

func TestManager_ResumeStrict_UnknownDatabase(t *testing.T) {
	mgr := newTestManager("a")
	_, err := mgr.ResumeStrict("missing")
	if !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("error = %v, want ErrDatabaseNotFound", err)
	}
}

func TestManager_Instances_ReturnsCopy(t *testing.T) {
	mgr := newTestManager("a")
	cp := mgr.Instances()
	cp["fake"] = nil
	if mgr.InstanceCount() != 1 {
		t.Error("original should not be modified")
	}
}

func TestManager_FleetStatus_Mode(t *testing.T) {
	cfg := &config.Config{Mode: "standalone"}
	mgr := NewManager(cfg)
	status := mgr.FleetStatus()
	if status.Mode != "standalone" {
		t.Errorf("expected standalone, got %s", status.Mode)
	}
}

func TestManager_PoolForDatabase_Named(t *testing.T) {
	mgr := newTestManager("db1", "db2")
	inst := mgr.GetInstance("db1")
	got := mgr.PoolForDatabase("db1")
	if got != inst.Pool {
		t.Error("expected pool from db1 instance")
	}
}

func TestManager_PoolForDatabase_Empty(t *testing.T) {
	mgr := newTestManager("db1")
	got := mgr.PoolForDatabase("")
	if got != mgr.GetInstance("db1").Pool {
		t.Error("expected first available pool")
	}
}

func TestManager_PoolForDatabase_All(t *testing.T) {
	mgr := newTestManager("db1")
	got := mgr.PoolForDatabase("all")
	if got != mgr.GetInstance("db1").Pool {
		t.Error("expected first available pool for 'all'")
	}
}

func TestManager_PoolForDatabase_Unknown(t *testing.T) {
	mgr := newTestManager("db1")
	if mgr.PoolForDatabase("nope") != nil {
		t.Error("expected nil for unknown database")
	}
}

func TestManager_PoolForDatabase_EmptyManager(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := NewManager(cfg)
	if mgr.PoolForDatabase("") != nil {
		t.Error("expected nil for empty manager")
	}
}

func TestManager_FleetStatus_Tags(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := NewManager(cfg)
	mgr.RegisterInstance(&DatabaseInstance{
		Name: "prod",
		Config: config.DatabaseConfig{
			Name: "prod",
			Tags: []string{"critical", "us-east"},
		},
		Status: &InstanceStatus{Connected: true},
	})
	status := mgr.FleetStatus()
	if len(status.Databases[0].Tags) != 2 {
		t.Errorf("tags: %v", status.Databases[0].Tags)
	}
}
