package api

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// CHECK-27: /api/v1/databases returns log_source.state for every database.
func TestAPI_Databases_LogSourceStatePerDatabase(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "active",
		Config: config.DatabaseConfig{Name: "active"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()},
		LogSource: func() logwatch.SourceStatus {
			return logwatch.SourceStatus{State: logwatch.SourceActive,
				File: "/pg/log/postgresql.json", Format: "jsonlog"}
		}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "stderr",
		Config: config.DatabaseConfig{Name: "stderr"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()},
		LogSource: func() logwatch.SourceStatus {
			return logwatch.UnavailableStatus(`log_destination is "stderr"`)
		}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "plain",
		Config: config.DatabaseConfig{Name: "plain"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()}})
	w := get(t, NewRouter(mgr, cfg, nil, fakeAdminMiddleware), "/api/v1/databases")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	want := map[string]string{"active": "active", "stderr": "unavailable", "plain": "disabled"}
	dbs, _ := decodeJSON(t, w)["databases"].([]any)
	if len(dbs) != len(want) {
		t.Fatalf("databases = %d, want %d", len(dbs), len(want))
	}
	for _, raw := range dbs {
		db, _ := raw.(map[string]any)
		status, _ := db["status"].(map[string]any)
		src, ok := status["log_source"].(map[string]any)
		name, _ := db["name"].(string)
		if !ok {
			t.Fatalf("database %q has no log_source: %v", name, status)
		}
		if src["state"] != want[name] {
			t.Fatalf("database %q log_source = %v, want state %s", name, src, want[name])
		}
		if name == "active" && src["file"] != "/pg/log/postgresql.json" {
			t.Fatalf("active log_source lacks the file: %v", src)
		}
		if reason, _ := src["reason"].(string); name != "active" && reason == "" {
			t.Fatalf("database %q log_source has no reason: %v", name, src)
		}
	}
}
