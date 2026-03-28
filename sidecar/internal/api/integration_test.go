//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func setupIntegrationRouter() (http.Handler, *fleet.DatabaseManager) {
	cfg := &config.Config{
		Mode:  "fleet",
		Trust: config.TrustConfig{Level: "advisory"},
		Collector: config.CollectorConfig{
			IntervalSeconds: 60, BatchSize: 1000,
		},
		Analyzer: config.AnalyzerConfig{IntervalSeconds: 600},
		Safety: config.SafetyConfig{
			CPUCeilingPct: 90, QueryTimeoutMs: 500,
		},
	}
	mgr := fleet.NewManager(cfg)

	// Register databases with varying health
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod-orders",
		Config: config.DatabaseConfig{
			Name: "prod-orders",
			Tags: []string{"production", "critical"},
		},
		Status: &fleet.InstanceStatus{
			Connected:        true,
			PGVersion:        "16.2",
			TrustLevel:       "advisory",
			FindingsOpen:     3,
			FindingsCritical: 1,
			FindingsWarning:  2,
			ActionsTotal:     12,
			LastSeen:         time.Now(),
		},
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod-users",
		Config: config.DatabaseConfig{
			Name: "prod-users",
			Tags: []string{"production"},
		},
		Status: &fleet.InstanceStatus{
			Connected:    true,
			PGVersion:    "16.2",
			TrustLevel:   "observation",
			FindingsOpen: 1,
			FindingsInfo: 1,
			LastSeen:     time.Now(),
		},
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "staging",
		Config: config.DatabaseConfig{Name: "staging"},
		Status: &fleet.InstanceStatus{
			Connected: true,
			Error:     "replication lag > 10s",
			LastSeen:  time.Now(),
		},
	})

	return NewRouter(mgr, cfg, nil), mgr
}

func TestIntegration_FleetDashboardData(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest("GET", "/api/v1/databases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	summary := resp["summary"].(map[string]any)
	if summary["total_databases"].(float64) != 3 {
		t.Errorf("total databases: %v", summary["total_databases"])
	}
	if summary["healthy"].(float64) != 2 {
		t.Errorf("healthy: %v", summary["healthy"])
	}
	if summary["degraded"].(float64) != 1 {
		t.Errorf("degraded: %v", summary["degraded"])
	}
	if summary["total_critical"].(float64) != 1 {
		t.Errorf("critical: %v", summary["total_critical"])
	}
}

func TestIntegration_DatabasesSortedByHealth(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest("GET", "/api/v1/databases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	dbs := resp["databases"].([]any)

	// staging (error, score=0) should be first
	first := dbs[0].(map[string]any)
	if first["name"] != "staging" {
		t.Errorf("expected staging first (worst health), got %s", first["name"])
	}
}

func TestIntegration_ConfigGetReturnsFleetConfig(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest("GET", "/api/v1/config", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["mode"] != "fleet" {
		t.Errorf("mode: %v", resp["mode"])
	}
}

func TestIntegration_ConfigGetPerDatabase(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest("GET", "/api/v1/config?database=prod-orders", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["database"] != "prod-orders" {
		t.Errorf("database: %v", resp["database"])
	}
}

func TestIntegration_EmergencyStopStopsExecutor(t *testing.T) {
	r, mgr := setupIntegrationRouter()

	// Stop prod-orders
	req := httptest.NewRequest("POST", "/api/v1/emergency-stop?database=prod-orders", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["stopped"].(float64) != 1 {
		t.Errorf("stopped: %v", resp["stopped"])
	}

	if !mgr.GetInstance("prod-orders").Stopped {
		t.Error("prod-orders should be stopped")
	}
	if mgr.GetInstance("prod-users").Stopped {
		t.Error("prod-users should not be stopped")
	}

	// Resume
	req = httptest.NewRequest("POST", "/api/v1/resume?database=prod-orders", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if mgr.GetInstance("prod-orders").Stopped {
		t.Error("prod-orders should be resumed")
	}
}

func TestIntegration_ConfigUpdateTrustLevel(t *testing.T) {
	r, _ := setupIntegrationRouter()

	body := `{"trust":{"level":"autonomous"}}`
	req := httptest.NewRequest("PUT", "/api/v1/config",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	// Verify change took effect
	req = httptest.NewRequest("GET", "/api/v1/config", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	trust := resp["trust"].(map[string]any)
	if trust["Level"] != "autonomous" {
		t.Errorf("trust level: %v", trust["Level"])
	}
}

func TestIntegration_MetricsFleetWide(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest("GET", "/api/v1/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["fleet"] == nil {
		t.Error("missing fleet summary in metrics")
	}
	if resp["databases"] == nil {
		t.Error("missing databases in metrics")
	}
}

func TestIntegration_MetricsPerDatabase(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest("GET", "/api/v1/metrics?database=prod-users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["database"] != "prod-users" {
		t.Errorf("database: %v", resp["database"])
	}
}

func TestIntegration_CORSPreflight(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest(
		"OPTIONS", "/api/v1/databases", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("OPTIONS status: %d", w.Code)
	}
	got := w.Header().Get("Access-Control-Allow-Origin")
	if got != "http://localhost:8080" {
		t.Errorf("CORS origin: got %q", got)
	}
}

func TestIntegration_DashboardServed(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "pg_sage") && !strings.Contains(body, "root") {
		t.Error("dashboard HTML not served")
	}
}
