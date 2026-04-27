package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// --- configGlobalGetHandler ---
// Requires a real ConfigStore (backed by pgxpool). We test that the
// handler correctly returns errors from the store.

func TestConfigGlobalGetHandler_NilStore(t *testing.T) {
	// ConfigStore with nil pool will panic on GetMergedConfig.
	// This confirms the handler does call through to the store.
	// Integration tests cover the full path.
	//
	// No unit test possible without interface-based store.
}

// --- configGlobalPutHandler ---

func TestConfigGlobalPutHandler_MalformedJSON(t *testing.T) {
	cfg := &config.Config{}
	// nil ConfigStore -- we never reach it because JSON parse
	// fails first.
	handler := configGlobalPutHandler(nil, cfg, nil)

	w := doRequest(
		handler, "PUT", "/api/v1/config/global",
		"not valid json")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid JSON" {
		t.Errorf("error: got %q, want 'invalid JSON'",
			resp["error"])
	}
}

func TestConfigGlobalPutHandler_EmptyObject(t *testing.T) {
	cfg := &config.Config{}
	handler := configGlobalPutHandler(nil, cfg, nil)

	// Empty object means no keys to iterate, so
	// applyConfigOverrides returns no errors.
	w := doRequest(
		handler, "PUT", "/api/v1/config/global", "{}")

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "updated" {
		t.Errorf("status: got %q, want 'updated'",
			resp["status"])
	}
}

func TestConfigGlobalPutHandler_ExtractsUserFromContext(
	t *testing.T,
) {
	cfg := &config.Config{}
	handler := configGlobalPutHandler(nil, cfg, nil)

	// With a user in context and empty body, should succeed.
	w := doRequestWithUser(
		handler, "PUT", "/api/v1/config/global", "{}",
		testAdminUser())

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
}

func TestConfigGlobalPutHandler_ContentType(t *testing.T) {
	cfg := &config.Config{}
	handler := configGlobalPutHandler(nil, cfg, nil)
	w := doRequest(
		handler, "PUT", "/api/v1/config/global",
		"not json")

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json",
			ct)
	}
}

func TestConfigGlobalDeleteHandler_RejectsExecutionMode(t *testing.T) {
	cfg := &config.Config{}
	handler := configGlobalDeleteHandler(nil, cfg, cfg, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/config/global/{key}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/config/global/execution_mode", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid config key" {
		t.Errorf("error: got %q, want invalid config key",
			resp["error"])
	}
}

func TestConfigGlobalDeleteHandler_RejectsUnknownKey(t *testing.T) {
	cfg := &config.Config{}
	handler := configGlobalDeleteHandler(nil, cfg, cfg, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/config/global/{key}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/config/global/unknown.key", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid config key" {
		t.Errorf("error: got %q, want invalid config key",
			resp["error"])
	}
}

// --- configDBGetHandler ---

func TestConfigDBGetHandler_InvalidID(t *testing.T) {
	cfg := &config.Config{}
	handler := configDBGetHandler(nil, cfg, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"GET /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"GET", "/api/v1/config/databases/abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid database id" {
		t.Errorf("error: got %q, want 'invalid database id'",
			resp["error"])
	}
}

func TestConfigDBGetHandler_ZeroID(t *testing.T) {
	cfg := &config.Config{}
	handler := configDBGetHandler(nil, cfg, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"GET /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"GET", "/api/v1/config/databases/0", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// 0 fails the dbID < 1 check.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid database id" {
		t.Errorf("error: got %q, want 'invalid database id'",
			resp["error"])
	}
}

func TestConfigDBGetHandler_NegativeID(t *testing.T) {
	cfg := &config.Config{}
	handler := configDBGetHandler(nil, cfg, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"GET /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"GET", "/api/v1/config/databases/-1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// --- configDBPutHandler ---

func TestConfigDBPutHandler_InvalidID(t *testing.T) {
	cfg := &config.Config{}
	handler := configDBPutHandler(nil, cfg, nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/config/databases/abc",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid database id" {
		t.Errorf("error: got %q, want 'invalid database id'",
			resp["error"])
	}
}

func TestConfigDBPutHandler_ZeroID(t *testing.T) {
	cfg := &config.Config{}
	handler := configDBPutHandler(nil, cfg, nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/config/databases/0",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

func TestConfigDBPutHandler_MalformedJSON(t *testing.T) {
	cfg := &config.Config{}
	handler := configDBPutHandler(nil, cfg, nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/config/databases/1",
		strings.NewReader("bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid JSON" {
		t.Errorf("error: got %q, want 'invalid JSON'",
			resp["error"])
	}
}

func TestConfigDBPutHandler_EmptyBody(t *testing.T) {
	cfg := &config.Config{}
	handler := configDBPutHandler(nil, cfg, nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/config/databases/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/config/databases/1",
		strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Empty body means no overrides to apply, should succeed.
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "updated" {
		t.Errorf("status: got %q, want 'updated'",
			resp["status"])
	}
}

func TestConfigDBPutHandler_FleetValidation_NotFound(
	t *testing.T,
) {
	cfg := &config.Config{}
	mgr := fleet.NewManager(cfg)
	// Register one instance with DatabaseID=1.
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "testdb",
		DatabaseID: 1,
		Config:     config.DatabaseConfig{Name: "testdb"},
		Status:     &fleet.InstanceStatus{},
	})

	handler := configDBPutHandler(nil, cfg, nil, mgr)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/config/databases/{id}", handler)

	// Request for DB ID 99 — does not exist.
	req := httptest.NewRequest(
		"PUT", "/api/v1/config/databases/99",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "database 99 not found" {
		t.Errorf("error: got %q, want 'database 99 not found'",
			resp["error"])
	}
}

func TestConfigDBPutHandler_FleetValidation_Found(
	t *testing.T,
) {
	cfg := &config.Config{}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "testdb",
		DatabaseID: 1,
		Config:     config.DatabaseConfig{Name: "testdb"},
		Status:     &fleet.InstanceStatus{},
	})

	handler := configDBPutHandler(nil, cfg, nil, mgr)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/config/databases/{id}", handler)

	// Request for DB ID 1 — exists, empty body → success.
	req := httptest.NewRequest(
		"PUT", "/api/v1/config/databases/1",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
}

func TestSyncTrustLevelToFleet(t *testing.T) {
	cfg := &config.Config{}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "db1",
		DatabaseID: 1,
		Config:     config.DatabaseConfig{Name: "db1"},
		Status: &fleet.InstanceStatus{
			TrustLevel: "advisory",
		},
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "db2",
		DatabaseID: 2,
		Config:     config.DatabaseConfig{Name: "db2"},
		Status: &fleet.InstanceStatus{
			TrustLevel: "advisory",
		},
	})

	syncTrustLevelToFleet(mgr, "autonomous")

	for _, name := range []string{"db1", "db2"} {
		inst := mgr.GetInstance(name)
		if inst == nil {
			t.Fatalf("instance %q not found", name)
		}
		if inst.Status.TrustLevel != "autonomous" {
			t.Errorf("db %q: trust level got %q, want autonomous",
				name, inst.Status.TrustLevel)
		}
	}
}

func TestSyncDatabaseTrustLevelResetClearsExecutorOverride(
	t *testing.T,
) {
	cfg := &config.Config{
		Trust: config.TrustConfig{Level: "advisory"},
	}
	exec := executor.New(nil, cfg, nil, time.Now(),
		func(string, string, ...any) {})
	if err := exec.SetTrustLevel("autonomous"); err != nil {
		t.Fatalf("set trust override: %v", err)
	}

	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:       "db1",
		DatabaseID: 1,
		Config:     config.DatabaseConfig{Name: "db1"},
		Executor:   exec,
		Status: &fleet.InstanceStatus{
			TrustLevel: "autonomous",
		},
	})

	syncDatabaseTrustLevelReset(mgr, cfg, 1)

	if got := exec.TrustLevel(); got != "advisory" {
		t.Fatalf("executor trust level = %q, want advisory", got)
	}
	inst := mgr.GetInstance("db1")
	if got := inst.SnapshotStatus().TrustLevel; got != "advisory" {
		t.Fatalf("status trust level = %q, want advisory", got)
	}
}

// --- configAuditHandler ---
// Requires a real ConfigStore. No request validation to test
// (GET with optional limit param). We test parseIntDefault below.

func TestValidateDatabaseParam(t *testing.T) {
	valid := []string{
		"",
		"all",
		"db1",
		"my_database",
		"my-database",
		"my.database",
		"DB_PROD_1",
		strings.Repeat("a", 63),
	}
	for _, s := range valid {
		if err := validateDatabaseParam(s); err != nil {
			t.Errorf("validateDatabaseParam(%q) = %v, want nil",
				s, err)
		}
	}

	invalid := []string{
		strings.Repeat("a", 64), // too long
		"db'1",                  // quote
		"db;DROP",               // semicolon
		"db 1",                  // space
		"db/1",                  // slash
		"db\\1",                 // backslash
		"db$1",                  // dollar
		"db<script>",            // angle brackets
		"db\n1",                 // newline
		"db\t1",                 // tab
		"db\x001",               // null byte
		"db%1",                  // percent
	}
	for _, s := range invalid {
		if err := validateDatabaseParam(s); err == nil {
			t.Errorf("validateDatabaseParam(%q) = nil, want error",
				s)
		}
	}
}

func TestTimeoutMiddleware_AttachesDeadline(t *testing.T) {
	var (
		sawDeadline bool
		err         error
	)
	h := timeoutMiddleware(http.HandlerFunc(func(
		_ http.ResponseWriter, r *http.Request,
	) {
		_, ok := r.Context().Deadline()
		sawDeadline = ok
		err = r.Context().Err()
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !sawDeadline {
		t.Error("handler did not see a context deadline")
	}
	if err != nil {
		t.Errorf("context was already cancelled: %v", err)
	}
}

func TestTimeoutMiddleware_CancelsAfterDeadline(t *testing.T) {
	// Use the real middleware but override the base request
	// context to a very short-lived parent so we don't need to
	// sleep for the full defaultRequestTimeout.
	ctx, cancel := context.WithTimeout(
		context.Background(), 1*time.Millisecond)
	defer cancel()

	var ctxErr error
	h := timeoutMiddleware(http.HandlerFunc(func(
		_ http.ResponseWriter, r *http.Request,
	) {
		// Wait until the parent deadline fires; the wrapped
		// context should be cancelled too (WithTimeout derives
		// from r.Context() so the shorter deadline wins).
		<-r.Context().Done()
		ctxErr = r.Context().Err()
	}))
	req := httptest.NewRequest("GET", "/x", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if ctxErr == nil {
		t.Fatal("context was not cancelled after deadline")
	}
}

func TestReadDatabaseParam_Invalid(t *testing.T) {
	req := httptest.NewRequest(
		"GET", "/x?database=bad%27drop", nil)
	w := httptest.NewRecorder()
	got, ok := readDatabaseParam(w, req)
	if ok {
		t.Fatalf("readDatabaseParam ok=true for bad input")
	}
	if got != "" {
		t.Errorf("got = %q, want empty", got)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestReadDatabaseParam_Valid(t *testing.T) {
	req := httptest.NewRequest(
		"GET", "/x?database=db1", nil)
	w := httptest.NewRecorder()
	got, ok := readDatabaseParam(w, req)
	if !ok {
		t.Fatalf("readDatabaseParam ok=false for valid input")
	}
	if got != "db1" {
		t.Errorf("got = %q, want db1", got)
	}
}

func TestReadDatabaseParam_Empty(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	w := httptest.NewRecorder()
	got, ok := readDatabaseParam(w, req)
	if !ok {
		t.Fatalf("readDatabaseParam ok=false for empty input")
	}
	if got != "" {
		t.Errorf("got = %q, want empty", got)
	}
}

func TestParseIntDefault(t *testing.T) {
	tests := []struct {
		input    string
		def      int
		expected int
	}{
		{"", 100, 100},
		{"50", 100, 50},
		{"abc", 100, 100},
		{"-1", 100, 100},
		{"0", 100, 0},
		{"200", 100, 200},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseIntDefault(tt.input, tt.def)
			if got != tt.expected {
				t.Errorf("parseIntDefault(%q, %d): got %d, "+
					"want %d", tt.input, tt.def,
					got, tt.expected)
			}
		})
	}
}

// --- hotReload tests ---

func TestHotReload_CollectorInterval(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "collector.interval_seconds", "30")
	if cfg.Collector.IntervalSeconds != 30 {
		t.Errorf("interval: got %d, want 30",
			cfg.Collector.IntervalSeconds)
	}
}

func TestHotReload_CollectorBatchSize(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "collector.batch_size", "500")
	if cfg.Collector.BatchSize != 500 {
		t.Errorf("batch_size: got %d, want 500",
			cfg.Collector.BatchSize)
	}
}

func TestHotReload_CollectorMaxQueries(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "collector.max_queries", "200")
	if cfg.Collector.MaxQueries != 200 {
		t.Errorf("max_queries: got %d, want 200",
			cfg.Collector.MaxQueries)
	}
}

func TestHotReload_AnalyzerFields(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "analyzer.interval_seconds", "120")
	if cfg.Analyzer.IntervalSeconds != 120 {
		t.Errorf("interval: got %d, want 120",
			cfg.Analyzer.IntervalSeconds)
	}

	hotReload(cfg, "analyzer.slow_query_threshold_ms", "5000")
	if cfg.Analyzer.SlowQueryThresholdMs != 5000 {
		t.Errorf("slow_query: got %d, want 5000",
			cfg.Analyzer.SlowQueryThresholdMs)
	}

	hotReload(cfg, "analyzer.seq_scan_min_rows", "10000")
	if cfg.Analyzer.SeqScanMinRows != 10000 {
		t.Errorf("seq_scan: got %d, want 10000",
			cfg.Analyzer.SeqScanMinRows)
	}

	hotReload(cfg, "analyzer.unused_index_window_days", "7")
	if cfg.Analyzer.UnusedIndexWindowDays != 7 {
		t.Errorf("unused_index: got %d, want 7",
			cfg.Analyzer.UnusedIndexWindowDays)
	}

	hotReload(cfg, "analyzer.cache_hit_ratio_warning", "0.95")
	if cfg.Analyzer.CacheHitRatioWarning != 0.95 {
		t.Errorf("cache_hit: got %f, want 0.95",
			cfg.Analyzer.CacheHitRatioWarning)
	}
}

func TestHotReload_TrustLevel(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "trust.level", "autonomous")
	if cfg.Trust.Level != "autonomous" {
		t.Errorf("level: got %q, want autonomous",
			cfg.Trust.Level)
	}
}

func TestHotReload_TrustBooleans(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "trust.tier3_safe", "true")
	if !cfg.Trust.Tier3Safe {
		t.Error("tier3_safe should be true")
	}

	hotReload(cfg, "trust.tier3_moderate", "true")
	if !cfg.Trust.Tier3Moderate {
		t.Error("tier3_moderate should be true")
	}

	hotReload(cfg, "trust.tier3_high_risk", "false")
	if cfg.Trust.Tier3HighRisk {
		t.Error("tier3_high_risk should be false")
	}
}

func TestHotReload_TrustMaintenanceWindow(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "trust.maintenance_window", "02:00-04:00")
	if cfg.Trust.MaintenanceWindow != "02:00-04:00" {
		t.Errorf("window: got %q, want 02:00-04:00",
			cfg.Trust.MaintenanceWindow)
	}
}

func TestHotReload_SafetyFields(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "safety.cpu_ceiling_pct", "85")
	if cfg.Safety.CPUCeilingPct != 85 {
		t.Errorf("cpu: got %d, want 85",
			cfg.Safety.CPUCeilingPct)
	}

	hotReload(cfg, "safety.query_timeout_ms", "3000")
	if cfg.Safety.QueryTimeoutMs != 3000 {
		t.Errorf("query_timeout: got %d, want 3000",
			cfg.Safety.QueryTimeoutMs)
	}

	hotReload(cfg, "safety.ddl_timeout_seconds", "60")
	if cfg.Safety.DDLTimeoutSeconds != 60 {
		t.Errorf("ddl_timeout: got %d, want 60",
			cfg.Safety.DDLTimeoutSeconds)
	}

	hotReload(cfg, "safety.lock_timeout_ms", "5000")
	if cfg.Safety.LockTimeoutMs != 5000 {
		t.Errorf("lock_timeout: got %d, want 5000",
			cfg.Safety.LockTimeoutMs)
	}
}

func TestHotReload_LLMFields(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "llm.enabled", "true")
	if !cfg.LLM.Enabled {
		t.Error("llm.enabled should be true")
	}

	hotReload(cfg, "llm.endpoint", "https://api.example.com")
	if cfg.LLM.Endpoint != "https://api.example.com" {
		t.Errorf("endpoint: got %q",
			cfg.LLM.Endpoint)
	}

	hotReload(cfg, "llm.model", "gemini-pro")
	if cfg.LLM.Model != "gemini-pro" {
		t.Errorf("model: got %q", cfg.LLM.Model)
	}

	hotReload(cfg, "llm.timeout_seconds", "30")
	if cfg.LLM.TimeoutSeconds != 30 {
		t.Errorf("timeout: got %d, want 30",
			cfg.LLM.TimeoutSeconds)
	}

	hotReload(cfg, "llm.token_budget_daily", "10000")
	if cfg.LLM.TokenBudgetDaily != 10000 {
		t.Errorf("budget: got %d, want 10000",
			cfg.LLM.TokenBudgetDaily)
	}

	hotReload(cfg, "llm.context_budget_tokens", "4096")
	if cfg.LLM.ContextBudgetTokens != 4096 {
		t.Errorf("context_budget: got %d, want 4096",
			cfg.LLM.ContextBudgetTokens)
	}
}

func TestHotReload_AlertingFields(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "alerting.enabled", "true")
	if !cfg.Alerting.Enabled {
		t.Error("alerting.enabled should be true")
	}

	hotReload(cfg, "alerting.slack_webhook_url",
		"https://hooks.slack.com/test")
	if cfg.Alerting.SlackWebhookURL !=
		"https://hooks.slack.com/test" {
		t.Errorf("slack_webhook: got %q",
			cfg.Alerting.SlackWebhookURL)
	}

	hotReload(cfg, "alerting.check_interval_seconds", "30")
	if cfg.Alerting.CheckIntervalSeconds != 30 {
		t.Errorf("check_interval: got %d, want 30",
			cfg.Alerting.CheckIntervalSeconds)
	}

	hotReload(cfg, "alerting.cooldown_minutes", "15")
	if cfg.Alerting.CooldownMinutes != 15 {
		t.Errorf("cooldown: got %d, want 15",
			cfg.Alerting.CooldownMinutes)
	}

	hotReload(cfg, "alerting.quiet_hours_start", "22:00")
	if cfg.Alerting.QuietHoursStart != "22:00" {
		t.Errorf("quiet_start: got %q",
			cfg.Alerting.QuietHoursStart)
	}

	hotReload(cfg, "alerting.quiet_hours_end", "06:00")
	if cfg.Alerting.QuietHoursEnd != "06:00" {
		t.Errorf("quiet_end: got %q",
			cfg.Alerting.QuietHoursEnd)
	}
}

func TestHotReload_RetentionFields(t *testing.T) {
	cfg := &config.Config{}
	hotReload(cfg, "retention.snapshots_days", "30")
	if cfg.Retention.SnapshotsDays != 30 {
		t.Errorf("snapshots: got %d, want 30",
			cfg.Retention.SnapshotsDays)
	}

	hotReload(cfg, "retention.findings_days", "90")
	if cfg.Retention.FindingsDays != 90 {
		t.Errorf("findings: got %d, want 90",
			cfg.Retention.FindingsDays)
	}

	hotReload(cfg, "retention.actions_days", "180")
	if cfg.Retention.ActionsDays != 180 {
		t.Errorf("actions: got %d, want 180",
			cfg.Retention.ActionsDays)
	}

	hotReload(cfg, "retention.explains_days", "14")
	if cfg.Retention.ExplainsDays != 14 {
		t.Errorf("explains: got %d, want 14",
			cfg.Retention.ExplainsDays)
	}
}

func TestHotReload_UnknownKeyNoEffect(t *testing.T) {
	cfg := &config.Config{
		Collector: config.CollectorConfig{
			IntervalSeconds: 60,
		},
	}
	hotReload(cfg, "unknown.key", "value")
	if cfg.Collector.IntervalSeconds != 60 {
		t.Error("unknown key should not modify config")
	}
}

func TestHotReload_InvalidIntValue(t *testing.T) {
	cfg := &config.Config{}
	// atoi("not_int") returns 0, so field gets set to 0.
	hotReload(cfg, "collector.interval_seconds", "not_int")
	if cfg.Collector.IntervalSeconds != 0 {
		t.Errorf("interval: got %d, want 0",
			cfg.Collector.IntervalSeconds)
	}
}

// --- atoi / atof ---

func TestAtoi(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"0", 0},
		{"42", 42},
		{"-1", -1},
		{"abc", 0},
		{"", 0},
		{"3.14", 0},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := atoi(tt.input)
			if got != tt.expected {
				t.Errorf("atoi(%q): got %d, want %d",
					tt.input, got, tt.expected)
			}
		})
	}
}

func TestAtof(t *testing.T) {
	tests := []struct {
		input    string
		expected float64
	}{
		{"0.0", 0.0},
		{"3.14", 3.14},
		{"-1.5", -1.5},
		{"abc", 0.0},
		{"", 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := atof(tt.input)
			if got != tt.expected {
				t.Errorf("atof(%q): got %f, want %f",
					tt.input, got, tt.expected)
			}
		})
	}
}
