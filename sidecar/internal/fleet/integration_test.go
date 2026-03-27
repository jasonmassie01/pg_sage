//go:build integration

package fleet

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestIntegration_FleetManager_StandaloneMode(t *testing.T) {
	cfg := &config.Config{
		Mode: "standalone",
		Databases: []config.DatabaseConfig{{
			Name:     "test",
			Host:     "localhost",
			Port:     5432,
			User:     "postgres",
			Password: "postgres",
			Database: "postgres",
			SSLMode:  "disable",
		}},
	}
	mgr := NewManager(cfg)
	mgr.RegisterInstance(&DatabaseInstance{
		Name:   "test",
		Config: cfg.Databases[0],
		Status: &InstanceStatus{
			Connected: true,
			LastSeen:  time.Now(),
		},
	})

	status := mgr.FleetStatus()
	if status.Mode != "standalone" {
		t.Errorf("mode: %s", status.Mode)
	}
	if status.Summary.TotalDatabases != 1 {
		t.Errorf("total: %d", status.Summary.TotalDatabases)
	}
	if status.Summary.Healthy != 1 {
		t.Errorf("healthy: %d", status.Summary.Healthy)
	}
}

func TestIntegration_FleetManager_MultiDatabase(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := NewManager(cfg)

	for _, name := range []string{"db1", "db2", "db3"} {
		mgr.RegisterInstance(&DatabaseInstance{
			Name:   name,
			Config: config.DatabaseConfig{Name: name},
			Status: &InstanceStatus{
				Connected:   true,
				LastSeen:    time.Now(),
				TrustLevel:  "advisory",
				HealthScore: 100,
			},
		})
	}

	// Degrade one
	mgr.GetInstance("db2").Status.FindingsCritical = 2
	mgr.GetInstance("db2").Status.FindingsOpen = 5

	status := mgr.FleetStatus()
	if status.Summary.TotalDatabases != 3 {
		t.Errorf("total: %d", status.Summary.TotalDatabases)
	}
	if status.Summary.TotalFindings != 5 {
		t.Errorf("findings: %d", status.Summary.TotalFindings)
	}
	if status.Summary.TotalCritical != 2 {
		t.Errorf("critical: %d", status.Summary.TotalCritical)
	}

	// Worst first
	if status.Databases[0].Name != "db2" {
		t.Errorf("expected db2 first (worst health), got %s", status.Databases[0].Name)
	}
}

func TestIntegration_EmergencyStopAndResume(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := NewManager(cfg)
	for _, name := range []string{"a", "b"} {
		mgr.RegisterInstance(&DatabaseInstance{
			Name:   name,
			Config: config.DatabaseConfig{Name: name},
			Status: &InstanceStatus{Connected: true, LastSeen: time.Now()},
		})
	}

	mgr.EmergencyStop("a")
	if !mgr.GetInstance("a").Stopped {
		t.Error("a should be stopped")
	}
	if mgr.GetInstance("b").Stopped {
		t.Error("b should not be stopped")
	}

	mgr.Resume("")
	if mgr.GetInstance("a").Stopped {
		t.Error("a should be resumed")
	}
}
