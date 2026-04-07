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

	return NewRouter(mgr, cfg, nil, fakeAdminMiddleware), mgr
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
	req.Header.Set("Content-Type", "application/json")
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
	req.Header.Set("Content-Type", "application/json")
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

// --- Forecasts in fleet context ---

func TestIntegration_ForecastsFleetWide(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest(
		"GET", "/api/v1/forecasts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["forecasts"]; !ok {
		t.Error("missing forecasts key")
	}
	// No pool in test → empty array, but key must exist.
	forecasts := resp["forecasts"].([]any)
	if len(forecasts) != 0 {
		t.Errorf("forecasts: got %d, want 0", len(forecasts))
	}
}

func TestIntegration_ForecastsPerDatabase(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest(
		"GET", "/api/v1/forecasts?database=prod-orders", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["database"] != "prod-orders" {
		t.Errorf("database: got %v, want prod-orders",
			resp["database"])
	}
}

// --- Query Hints in fleet context ---

func TestIntegration_QueryHintsFleetWide(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest(
		"GET", "/api/v1/query-hints", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["hints"]; !ok {
		t.Error("missing hints key")
	}
	hints := resp["hints"].([]any)
	if len(hints) != 0 {
		t.Errorf("hints: got %d, want 0", len(hints))
	}
}

func TestIntegration_QueryHintsPerDatabase(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest(
		"GET", "/api/v1/query-hints?database=prod-users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["database"] != "prod-users" {
		t.Errorf("database: got %v, want prod-users",
			resp["database"])
	}
}

// --- Alert Log in fleet context ---

func TestIntegration_AlertLogFleetWide(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest(
		"GET", "/api/v1/alert-log", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["alerts"]; !ok {
		t.Error("missing alerts key")
	}
	alerts := resp["alerts"].([]any)
	if len(alerts) != 0 {
		t.Errorf("alerts: got %d, want 0", len(alerts))
	}
}

func TestIntegration_AlertLogPerDatabase(t *testing.T) {
	r, _ := setupIntegrationRouter()
	req := httptest.NewRequest(
		"GET", "/api/v1/alert-log?database=staging", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["database"] != "staging" {
		t.Errorf("database: got %v, want staging",
			resp["database"])
	}
}

// --- Full fleet endpoint audit ---

func TestIntegration_AllPublicEndpoints(t *testing.T) {
	r, _ := setupIntegrationRouter()

	endpoints := []struct {
		method     string
		path       string
		wantStatus int
	}{
		{"GET", "/api/v1/databases", 200},
		{"GET", "/api/v1/findings", 200},
		{"GET", "/api/v1/findings?database=prod-orders", 200},
		{"GET", "/api/v1/actions", 200},
		{"GET", "/api/v1/actions?database=prod-orders", 200},
		{"GET", "/api/v1/forecasts", 200},
		{"GET", "/api/v1/forecasts?database=prod-orders", 200},
		{"GET", "/api/v1/query-hints", 200},
		{"GET", "/api/v1/query-hints?database=prod-users", 200},
		{"GET", "/api/v1/alert-log", 200},
		{"GET", "/api/v1/alert-log?database=staging", 200},
		{"GET", "/api/v1/snapshots/latest", 200},
		{"GET", "/api/v1/snapshots/latest?database=prod-orders", 200},
		{"GET", "/api/v1/config", 200},
		{"GET", "/api/v1/config?database=prod-orders", 200},
		{"GET", "/api/v1/metrics", 200},
		{"GET", "/api/v1/metrics?database=prod-orders", 200},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+"_"+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != ep.wantStatus {
				t.Errorf("%s %s: got %d, want %d",
					ep.method, ep.path,
					w.Code, ep.wantStatus)
			}
			ct := w.Header().Get("Content-Type")
			if ct != "application/json" {
				t.Errorf("%s %s: Content-Type: got %q",
					ep.method, ep.path, ct)
			}
		})
	}
}
