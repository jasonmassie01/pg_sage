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

func testRouter(databases ...string) http.Handler {
	cfg := &config.Config{
		Mode: "fleet",
		Trust: config.TrustConfig{Level: "advisory"},
		Collector: config.CollectorConfig{
			IntervalSeconds: 60,
			BatchSize:       1000,
		},
		Analyzer: config.AnalyzerConfig{IntervalSeconds: 600},
		Safety:   config.SafetyConfig{CPUCeilingPct: 90},
	}
	mgr := fleet.NewManager(cfg)
	for _, name := range databases {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name,
			Config: config.DatabaseConfig{
				Name: name,
				Tags: []string{"test"},
			},
			Status: &fleet.InstanceStatus{
				Connected: true,
				LastSeen:  time.Now(),
			},
		})
	}
	return NewRouter(mgr, cfg, nil)
}

func get(
	t *testing.T, handler http.Handler, path string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func post(
	t *testing.T, handler http.Handler, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func put(
	t *testing.T, handler http.Handler, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", path,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func decodeJSON(
	t *testing.T, w *httptest.ResponseRecorder,
) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, w.Body.String())
	}
	return m
}

// --- Databases endpoint ---

func TestAPI_Databases_SingleDB(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/databases")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	dbs := m["databases"].([]any)
	if len(dbs) != 1 {
		t.Errorf("databases: %d", len(dbs))
	}
	summary := m["summary"].(map[string]any)
	if summary["total_databases"].(float64) != 1 {
		t.Errorf("total: %v", summary["total_databases"])
	}
}

func TestAPI_Databases_Fleet(t *testing.T) {
	r := testRouter("a", "b", "c")
	w := get(t, r, "/api/v1/databases")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	dbs := m["databases"].([]any)
	if len(dbs) != 3 {
		t.Errorf("databases: %d", len(dbs))
	}
}

func TestAPI_Databases_IncludesDegraded(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "bad",
		Config: config.DatabaseConfig{Name: "bad"},
		Status: &fleet.InstanceStatus{
			Connected: true,
			Error:     "timeout",
		},
	})
	r := NewRouter(mgr, cfg, nil)
	w := get(t, r, "/api/v1/databases")
	m := decodeJSON(t, w)
	summary := m["summary"].(map[string]any)
	if summary["degraded"].(float64) != 1 {
		t.Errorf("degraded: %v", summary["degraded"])
	}
}

// --- Findings endpoint ---

func TestAPI_Findings_DefaultParams(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	filters := m["filters"].(map[string]any)
	if filters["Status"] != "open" {
		t.Errorf("status: %v", filters["Status"])
	}
	if m["limit"].(float64) != 50 {
		t.Errorf("limit: %v", m["limit"])
	}
}

func TestAPI_Findings_FilterByDatabase(t *testing.T) {
	r := testRouter("db1", "db2")
	w := get(t, r, "/api/v1/findings?database=db1")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: %v", m["database"])
	}
}

func TestAPI_Findings_FilterBySeverity(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings?severity=critical")
	m := decodeJSON(t, w)
	filters := m["filters"].(map[string]any)
	if filters["Severity"] != "critical" {
		t.Errorf("severity: %v", filters["Severity"])
	}
}

func TestAPI_Findings_FilterByCategory(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings?category=duplicate_index")
	m := decodeJSON(t, w)
	filters := m["filters"].(map[string]any)
	if filters["Category"] != "duplicate_index" {
		t.Errorf("category: %v", filters["Category"])
	}
}

func TestAPI_Findings_LimitCap(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings?limit=999")
	m := decodeJSON(t, w)
	if m["limit"].(float64) != 200 {
		t.Errorf("limit: %v", m["limit"])
	}
}

func TestAPI_Findings_Pagination(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings?limit=10&offset=20")
	m := decodeJSON(t, w)
	if m["offset"].(float64) != 20 {
		t.Errorf("offset: %v", m["offset"])
	}
	if m["limit"].(float64) != 10 {
		t.Errorf("limit: %v", m["limit"])
	}
}

func TestAPI_FindingDetail_NotFound(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/findings/99999")
	if w.Code != 404 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestAPI_Suppress(t *testing.T) {
	r := testRouter("db1")
	w := post(t, r, "/api/v1/findings/42/suppress", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["status"] != "suppressed" {
		t.Errorf("status: %v", m["status"])
	}
	if m["id"] != "42" {
		t.Errorf("id: %v", m["id"])
	}
}

func TestAPI_Unsuppress(t *testing.T) {
	r := testRouter("db1")
	w := post(t, r, "/api/v1/findings/42/unsuppress", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["status"] != "open" {
		t.Errorf("status: %v", m["status"])
	}
}

// --- Actions endpoint ---

func TestAPI_Actions_List(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/actions")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: got %v, want db1", m["database"])
	}
}

func TestAPI_Actions_FilterByDatabase(t *testing.T) {
	r := testRouter("db1", "db2")
	w := get(t, r, "/api/v1/actions?database=db1")
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: %v", m["database"])
	}
}

func TestAPI_Actions_LimitCap(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/actions?limit=999")
	m := decodeJSON(t, w)
	if m["limit"].(float64) != 200 {
		t.Errorf("limit: %v", m["limit"])
	}
}

func TestAPI_ActionDetail_NotFound(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/actions/99999")
	if w.Code != 404 {
		t.Errorf("status: %d", w.Code)
	}
}

// --- Snapshots endpoint ---

func TestAPI_SnapshotLatest_DefaultsToAll(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/snapshots/latest")
	if w.Code != 200 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestAPI_SnapshotLatest_UnknownDB(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/snapshots/latest?database=nope")
	if w.Code != 200 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestAPI_SnapshotLatest_OK(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/snapshots/latest?database=db1")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestAPI_SnapshotHistory_InvalidMetric(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r,
		"/api/v1/snapshots/history?database=db1&metric=bogus")
	if w.Code != 400 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestAPI_SnapshotHistory_OK(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r,
		"/api/v1/snapshots/history?database=db1&metric=cache_hit_ratio")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestAPI_SnapshotHistory_DefaultsToAll(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/snapshots/history?metric=tps")
	if w.Code != 200 {
		t.Errorf("status: %d", w.Code)
	}
}

// --- Config endpoint ---

func TestAPI_ConfigGet(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/config")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["mode"] != "fleet" {
		t.Errorf("mode: %v", m["mode"])
	}
}

func TestAPI_ConfigGet_PerDatabase(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/config?database=db1")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: %v", m["database"])
	}
}

func TestAPI_ConfigGet_PerDatabase_NotFound(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/config?database=nope")
	if w.Code != 404 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestAPI_ConfigUpdate(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"trust":{"level":"advisory"}}`)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["status"] != "updated" {
		t.Errorf("status: %v", m["status"])
	}
}

func TestAPI_ConfigUpdate_InvalidValue(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config",
		`{"trust":{"level":"invalid"}}`)
	if w.Code != 400 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestAPI_ConfigUpdate_InvalidJSON(t *testing.T) {
	r := testRouter("db1")
	w := put(t, r, "/api/v1/config", `not json`)
	if w.Code != 400 {
		t.Errorf("status: %d", w.Code)
	}
}

// --- Metrics endpoint ---

func TestAPI_Metrics(t *testing.T) {
	r := testRouter("db1", "db2")
	w := get(t, r, "/api/v1/metrics")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["fleet"] == nil {
		t.Error("missing fleet summary")
	}
}

func TestAPI_Metrics_PerDatabase(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/metrics?database=db1")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: %v", m["database"])
	}
}

func TestAPI_Metrics_NotFound(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/metrics?database=nope")
	if w.Code != 404 {
		t.Errorf("status: %d", w.Code)
	}
}

// --- Emergency controls ---

func TestAPI_EmergencyStop(t *testing.T) {
	r := testRouter("db1")
	w := post(t, r, "/api/v1/emergency-stop?database=db1", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["stopped"].(float64) != 1 {
		t.Errorf("stopped: %v", m["stopped"])
	}
}

func TestAPI_EmergencyStop_FleetWide(t *testing.T) {
	r := testRouter("a", "b", "c")
	w := post(t, r, "/api/v1/emergency-stop", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["stopped"].(float64) != 3 {
		t.Errorf("stopped: %v", m["stopped"])
	}
}

func TestAPI_Resume(t *testing.T) {
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "db1",
		Config: config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{Connected: true},
	})
	mgr.EmergencyStop("db1")
	r := NewRouter(mgr, cfg, nil)
	w := post(t, r, "/api/v1/resume?database=db1", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["resumed"].(float64) != 1 {
		t.Errorf("resumed: %v", m["resumed"])
	}
}

// --- CORS ---

func TestAPI_CORS_AllowedOrigin(t *testing.T) {
	r := testRouter("db1")
	req := httptest.NewRequest(
		"OPTIONS", "/api/v1/databases", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	got := w.Header().Get("Access-Control-Allow-Origin")
	if got != "http://localhost:8080" {
		t.Errorf("CORS origin: got %q, want localhost:8080", got)
	}
	if w.Code != 200 {
		t.Errorf("OPTIONS status: %d", w.Code)
	}
}

func TestAPI_CORS_DisallowedOrigin(t *testing.T) {
	r := testRouter("db1")
	req := httptest.NewRequest(
		"OPTIONS", "/api/v1/databases", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	got := w.Header().Get("Access-Control-Allow-Origin")
	if got != "" {
		t.Errorf("CORS should be empty for evil origin: %q", got)
	}
}

// --- Forecasts endpoint ---

func TestAPI_Forecasts_DefaultsToAll(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/forecasts")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	forecasts, ok := m["forecasts"].([]any)
	if !ok {
		t.Fatal("forecasts should be an array")
	}
	// No pool → empty array.
	if len(forecasts) != 0 {
		t.Errorf("forecasts: got %d, want 0", len(forecasts))
	}
}

func TestAPI_Forecasts_FilterByDatabase(t *testing.T) {
	r := testRouter("db1", "db2")
	w := get(t, r, "/api/v1/forecasts?database=db1")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: got %v, want db1", m["database"])
	}
}

func TestAPI_Forecasts_UnknownDB(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/forecasts?database=nope")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	forecasts := m["forecasts"].([]any)
	if len(forecasts) != 0 {
		t.Errorf("forecasts: got %d, want 0", len(forecasts))
	}
}

func TestAPI_Forecasts_ContentType(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/forecasts")
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
}

// --- Query Hints endpoint ---

func TestAPI_QueryHints_DefaultsToAll(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/query-hints")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	hints, ok := m["hints"].([]any)
	if !ok {
		t.Fatal("hints should be an array")
	}
	if len(hints) != 0 {
		t.Errorf("hints: got %d, want 0", len(hints))
	}
}

func TestAPI_QueryHints_FilterByDatabase(t *testing.T) {
	r := testRouter("db1", "db2")
	w := get(t, r, "/api/v1/query-hints?database=db1")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: got %v, want db1", m["database"])
	}
}

func TestAPI_QueryHints_UnknownDB(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/query-hints?database=nope")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	hints := m["hints"].([]any)
	if len(hints) != 0 {
		t.Errorf("hints: got %d, want 0", len(hints))
	}
}

func TestAPI_QueryHints_ContentType(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/query-hints")
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
}

// --- Alert Log endpoint ---

func TestAPI_AlertLog_DefaultsToAll(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/alert-log")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	alerts, ok := m["alerts"].([]any)
	if !ok {
		t.Fatal("alerts should be an array")
	}
	if len(alerts) != 0 {
		t.Errorf("alerts: got %d, want 0", len(alerts))
	}
}

func TestAPI_AlertLog_FilterByDatabase(t *testing.T) {
	r := testRouter("db1", "db2")
	w := get(t, r, "/api/v1/alert-log?database=db1")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	if m["database"] != "db1" {
		t.Errorf("database: got %v, want db1", m["database"])
	}
}

func TestAPI_AlertLog_UnknownDB(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/alert-log?database=nope")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	m := decodeJSON(t, w)
	alerts := m["alerts"].([]any)
	if len(alerts) != 0 {
		t.Errorf("alerts: got %d, want 0", len(alerts))
	}
}

func TestAPI_AlertLog_ContentType(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/alert-log")
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
}

// --- Route completeness ---

func TestAPI_AllEndpointsRegistered(t *testing.T) {
	r := testRouter("db1")

	// Every public API endpoint accessible without auth/pool
	// should return a non-404/405 status.
	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/databases"},
		{"GET", "/api/v1/findings"},
		{"GET", "/api/v1/actions"},
		{"GET", "/api/v1/forecasts"},
		{"GET", "/api/v1/query-hints"},
		{"GET", "/api/v1/alert-log"},
		{"GET", "/api/v1/snapshots/latest"},
		{"GET", "/api/v1/config"},
		{"GET", "/api/v1/metrics"},
	}
	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == 404 || w.Code == 405 {
			t.Errorf("%s %s: got %d (route not registered)",
				ep.method, ep.path, w.Code)
		}
	}
}

// --- SPA fallback ---

func TestAPI_Dashboard_Root(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "pg_sage") {
		t.Error("expected pg_sage in body")
	}
}
