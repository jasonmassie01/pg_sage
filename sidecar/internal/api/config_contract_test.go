package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// ================================================================
// Contract tests: replay realistic frontend payloads against the
// config API handlers. These validate that the backend accepts
// exactly what the Settings page sends.
//
// Background: the UI sends execution_mode alongside other config
// fields. The backend must silently strip execution_mode (it lives
// in sage.databases, not sage.config) instead of rejecting the
// entire payload with "unknown config key".
// ================================================================

// contractSetup returns a ConfigStore and a valid auth.User whose
// ID exists in sage.users (satisfying the FK on sage.config).
// Skips the test if Postgres is unavailable.
func contractSetup(
	t *testing.T,
) (*store.ConfigStore, *auth.User) {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	userID := phase2EnsureUser(t, pool, ctx)
	cs := store.NewConfigStore(pool)
	user := &auth.User{
		ID:    userID,
		Email: "phase2@test.com",
		Role:  auth.RoleAdmin,
	}
	return cs, user
}

// TestGlobalPut_MixedPayload_WithExecutionMode sends the exact
// payload that the Simple Mode tabs send: valid config fields mixed
// with execution_mode. The handler must strip execution_mode and
// persist the remaining valid fields.
func TestGlobalPut_MixedPayload_WithExecutionMode(
	t *testing.T,
) {
	cs, user := contractSetup(t)
	cfg := &config.Config{
		Trust:  config.TrustConfig{Level: "advisory"},
		Safety: config.SafetyConfig{CPUCeilingPct: 80},
	}
	handler := configGlobalPutHandler(cs, cfg, nil)

	body := `{
		"execution_mode": "auto",
		"trust.level": "autonomous",
		"safety.cpu_ceiling_pct": "90"
	}`

	w := doRequestWithUser(
		handler, "PUT", "/api/v1/config/global",
		body, user)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("status field: got %q, want 'updated'",
			resp["status"])
	}

	// Verify hot-reload applied the valid fields.
	if cfg.Trust.Level != "autonomous" {
		t.Errorf("trust.level: got %q, want 'autonomous'",
			cfg.Trust.Level)
	}
	if cfg.Safety.CPUCeilingPct != 90 {
		t.Errorf("safety.cpu_ceiling_pct: got %d, want 90",
			cfg.Safety.CPUCeilingPct)
	}
}

// TestGlobalPut_AllSimpleModeFields sends the full payload from
// the Simple Mode "Monitoring" tab: collector, analyzer, trust,
// safety, and execution_mode. All valid keys must be persisted.
func TestGlobalPut_AllSimpleModeFields(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := &config.Config{
		Collector: config.CollectorConfig{
			IntervalSeconds: 60,
		},
		Analyzer: config.AnalyzerConfig{
			SlowQueryThresholdMs:  1000,
			UnusedIndexWindowDays: 7,
		},
		Trust:  config.TrustConfig{Level: "advisory"},
		Safety: config.SafetyConfig{CPUCeilingPct: 80},
	}
	handler := configGlobalPutHandler(cs, cfg, nil)

	body := `{
		"collector.interval_seconds": "30",
		"analyzer.slow_query_threshold_ms": "500",
		"analyzer.unused_index_window_days": "14",
		"trust.level": "autonomous",
		"execution_mode": "auto",
		"safety.cpu_ceiling_pct": "85"
	}`

	w := doRequestWithUser(
		handler, "PUT", "/api/v1/config/global",
		body, user)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	// Verify every valid field was hot-reloaded.
	if cfg.Collector.IntervalSeconds != 30 {
		t.Errorf("collector.interval_seconds: got %d, want 30",
			cfg.Collector.IntervalSeconds)
	}
	if cfg.Analyzer.SlowQueryThresholdMs != 500 {
		t.Errorf(
			"analyzer.slow_query_threshold_ms: got %d, want 500",
			cfg.Analyzer.SlowQueryThresholdMs)
	}
	if cfg.Analyzer.UnusedIndexWindowDays != 14 {
		t.Errorf(
			"analyzer.unused_index_window_days: got %d, want 14",
			cfg.Analyzer.UnusedIndexWindowDays)
	}
	if cfg.Trust.Level != "autonomous" {
		t.Errorf("trust.level: got %q, want 'autonomous'",
			cfg.Trust.Level)
	}
	if cfg.Safety.CPUCeilingPct != 85 {
		t.Errorf("safety.cpu_ceiling_pct: got %d, want 85",
			cfg.Safety.CPUCeilingPct)
	}
}

// TestGlobalPut_AllAdvancedLLMFields sends the full payload from
// the Advanced Mode "LLM" tab: advisor and optimizer settings.
// All fields must be persisted and hot-reloaded.
func TestGlobalPut_AllAdvancedLLMFields(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := &config.Config{
		Advisor: config.AdvisorConfig{
			Enabled:         false,
			IntervalSeconds: 300,
		},
		LLM: config.LLMConfig{
			Optimizer: config.OptimizerConfig{
				Enabled:        false,
				MinQueryCalls:  10,
				MaxNewPerTable: 3,
			},
		},
	}
	handler := configGlobalPutHandler(cs, cfg, nil)

	body := `{
		"advisor.enabled": "true",
		"advisor.interval_seconds": "120",
		"llm.optimizer.enabled": "true",
		"llm.optimizer.min_query_calls": "5",
		"llm.optimizer.max_new_per_table": "2"
	}`

	w := doRequestWithUser(
		handler, "PUT", "/api/v1/config/global",
		body, user)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	// Verify hot-reload applied all LLM/advisor fields.
	if !cfg.Advisor.Enabled {
		t.Error("advisor.enabled: got false, want true")
	}
	if cfg.Advisor.IntervalSeconds != 120 {
		t.Errorf("advisor.interval_seconds: got %d, want 120",
			cfg.Advisor.IntervalSeconds)
	}
	if !cfg.LLM.Optimizer.Enabled {
		t.Error("llm.optimizer.enabled: got false, want true")
	}
	if cfg.LLM.Optimizer.MinQueryCalls != 5 {
		t.Errorf(
			"llm.optimizer.min_query_calls: got %d, want 5",
			cfg.LLM.Optimizer.MinQueryCalls)
	}
	if cfg.LLM.Optimizer.MaxNewPerTable != 2 {
		t.Errorf(
			"llm.optimizer.max_new_per_table: got %d, want 2",
			cfg.LLM.Optimizer.MaxNewPerTable)
	}
}

// TestGlobalPut_EmptyBody verifies that sending {} is a valid
// no-op (the UI may send empty saves).
func TestGlobalPut_EmptyBody(t *testing.T) {
	// No database needed: empty body means zero keys to iterate,
	// so applyConfigOverrides is never called with any key.
	cfg := &config.Config{}
	handler := configGlobalPutHandler(nil, cfg, nil)

	w := doRequest(
		handler, "PUT", "/api/v1/config/global", "{}")

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("status field: got %q, want 'updated'",
			resp["status"])
	}
}

// TestGlobalPut_SingleInvalidField verifies that a completely
// unknown config key returns 400. This differs from execution_mode
// which is silently stripped before validation.
func TestGlobalPut_SingleInvalidField(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := &config.Config{}
	handler := configGlobalPutHandler(cs, cfg, nil)

	body := `{"nonexistent.key": "value"}`

	w := doRequestWithUser(
		handler, "PUT", "/api/v1/config/global",
		body, user)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body: %s",
			w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] == "" {
		t.Error("expected a non-empty error message")
	}
}

// TestGlobalGet_ContainsAllExpectedKeys fetches the global config
// and verifies it includes keys from every config section the UI
// depends on.
func TestGlobalGet_ContainsAllExpectedKeys(t *testing.T) {
	cs, _ := contractSetup(t)
	cfg := &config.Config{
		Mode: "standalone",
		Trust: config.TrustConfig{
			Level: "advisory",
		},
	}
	handler := configGlobalGetHandler(cs, cfg)

	w := doRequest(handler, "GET", "/api/v1/config/global", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	var resp struct {
		Mode      string         `json:"mode"`
		Databases int            `json:"databases"`
		Config    map[string]any `json:"config"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Mode != "standalone" {
		t.Errorf("mode: got %q, want 'standalone'", resp.Mode)
	}

	// Every config section the UI relies on must be present.
	requiredKeys := []string{
		// Collector
		"collector.interval_seconds",
		"collector.batch_size",
		"collector.max_queries",
		// Analyzer
		"analyzer.interval_seconds",
		"analyzer.slow_query_threshold_ms",
		"analyzer.unused_index_window_days",
		// Trust
		"trust.level",
		"trust.tier3_safe",
		"trust.maintenance_window",
		// Safety
		"safety.cpu_ceiling_pct",
		"safety.query_timeout_ms",
		"safety.ddl_timeout_seconds",
		"safety.lock_timeout_ms",
		// LLM
		"llm.enabled",
		"llm.endpoint",
		"llm.model",
		"llm.optimizer.enabled",
		"llm.optimizer.min_query_calls",
		"llm.optimizer.max_new_per_table",
		// Advisor
		"advisor.enabled",
		"advisor.interval_seconds",
		// Alerting
		"alerting.enabled",
		"alerting.slack_webhook_url",
		"alerting.check_interval_seconds",
		// Retention
		"retention.snapshots_days",
		"retention.findings_days",
		"retention.actions_days",
		"retention.explains_days",
	}

	for _, key := range requiredKeys {
		if _, ok := resp.Config[key]; !ok {
			t.Errorf("missing expected config key: %s", key)
		}
	}

	if _, ok := resp.Config["execution_mode"]; ok {
		t.Fatal("global config should not expose database-only execution_mode")
	}
}

// TestGlobalPut_ExecutionModeOnly_StrippedSilently verifies that
// a payload containing ONLY execution_mode results in 200 (no-op
// after stripping), not 400.
func TestGlobalPut_ExecutionModeOnly_StrippedSilently(
	t *testing.T,
) {
	// After stripping execution_mode, body is empty -- no DB
	// call needed.
	cfg := &config.Config{}
	handler := configGlobalPutHandler(nil, cfg, nil)

	body := `{"execution_mode": "auto"}`

	w := doRequest(
		handler, "PUT", "/api/v1/config/global", body)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("status field: got %q, want 'updated'",
			resp["status"])
	}
}

// TestGlobalPut_MixedValidAndInvalid verifies that if the payload
// contains both valid config keys and one truly invalid key (not
// execution_mode), the handler returns 400. This ensures we
// distinguish between execution_mode (silently stripped) and
// genuinely unknown keys (rejected).
func TestGlobalPut_MixedValidAndInvalid(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := &config.Config{}
	handler := configGlobalPutHandler(cs, cfg, nil)

	body := `{
		"trust.level": "autonomous",
		"bogus.field": "whatever"
	}`

	w := doRequestWithUser(
		handler, "PUT", "/api/v1/config/global",
		body, user)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body: %s",
			w.Code, w.Body.String())
	}
}

// TestGlobalPut_RoundTrip_PersistsToStore verifies that values
// sent via PUT are actually persisted and visible in a subsequent
// GET.
func TestGlobalPut_RoundTrip_PersistsToStore(t *testing.T) {
	cs, user := contractSetup(t)
	cfg := &config.Config{
		Collector: config.CollectorConfig{
			IntervalSeconds: 60,
		},
	}

	putHandler := configGlobalPutHandler(cs, cfg, nil)
	getHandler := configGlobalGetHandler(cs, cfg)

	// PUT a new value.
	putBody := `{"collector.interval_seconds": "15"}`
	w := doRequestWithUser(
		putHandler, "PUT", "/api/v1/config/global",
		putBody, user)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	// GET should reflect the override.
	w = doRequest(getHandler, "GET", "/api/v1/config/global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	var resp struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	ci, ok := resp.Config["collector.interval_seconds"]
	if !ok {
		t.Fatal("collector.interval_seconds missing from GET")
	}
	ciMap, ok := ci.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", ci)
	}
	// After override, value should be 15 (int via coercion) and
	// source should be "override".
	val, _ := ciMap["value"].(float64)
	if int(val) != 15 {
		t.Errorf("value: got %v, want 15", ciMap["value"])
	}
	if ciMap["source"] != "override" {
		t.Errorf("source: got %v, want 'override'",
			ciMap["source"])
	}
}

// TestP2_ConfigGlobalPut_CompleteFrontendPayload sends a realistic
// payload spanning ALL config sections the frontend may send in a
// single save. Verifies PUT returns 200, then GET returns all
// fields with correct values and source=override.
func TestP2_ConfigGlobalPut_CompleteFrontendPayload(
	t *testing.T,
) {
	cs, user := contractSetup(t)
	cfg := &config.Config{
		Mode: "standalone",
		Collector: config.CollectorConfig{
			IntervalSeconds: 60,
			BatchSize:       1000,
			MaxQueries:      250,
		},
		Analyzer: config.AnalyzerConfig{
			IntervalSeconds:       300,
			SlowQueryThresholdMs:  1000,
			UnusedIndexWindowDays: 7,
		},
		Trust: config.TrustConfig{
			Level: "observation",
		},
		Safety: config.SafetyConfig{
			CPUCeilingPct:     80,
			QueryTimeoutMs:    5000,
			DDLTimeoutSeconds: 30,
			LockTimeoutMs:     500,
		},
		LLM: config.LLMConfig{
			Enabled:             false,
			Endpoint:            "",
			Model:               "gpt-4o-mini",
			TimeoutSeconds:      30,
			TokenBudgetDaily:    100000,
			ContextBudgetTokens: 4096,
			Optimizer: config.OptimizerConfig{
				Enabled:        false,
				MinQueryCalls:  10,
				MaxNewPerTable: 3,
			},
		},
		Advisor: config.AdvisorConfig{
			Enabled:         false,
			IntervalSeconds: 300,
		},
		Alerting: config.AlertingConfig{
			Enabled:              false,
			CheckIntervalSeconds: 300,
			CooldownMinutes:      60,
		},
		Retention: config.RetentionConfig{
			SnapshotsDays: 7,
			FindingsDays:  30,
			ActionsDays:   90,
			ExplainsDays:  14,
		},
	}

	putHandler := configGlobalPutHandler(cs, cfg, nil)
	getHandler := configGlobalGetHandler(cs, cfg)

	// Send a realistic payload mixing ALL overridable sections
	// plus execution_mode (which should be silently stripped).
	body := `{
		"execution_mode": "auto",
		"collector.interval_seconds": "15",
		"collector.batch_size": "500",
		"collector.max_queries": "100",
		"analyzer.slow_query_threshold_ms": "200",
		"analyzer.unused_index_window_days": "21",
		"trust.level": "autonomous",
		"trust.tier3_safe": "true",
		"trust.maintenance_window": "Sun 02:00-06:00",
		"trust.rollback_threshold_pct": "15",
		"safety.cpu_ceiling_pct": "95",
		"safety.query_timeout_ms": "10000",
		"safety.ddl_timeout_seconds": "120",
		"safety.lock_timeout_ms": "2000",
		"llm.enabled": "true",
		"llm.endpoint": "https://api.openai.com/v1",
		"llm.model": "gpt-4o",
		"llm.timeout_seconds": "60",
		"llm.token_budget_daily": "200000",
		"llm.context_budget_tokens": "8192",
		"llm.optimizer.enabled": "true",
		"llm.optimizer.min_query_calls": "3",
		"llm.optimizer.max_new_per_table": "5",
		"advisor.enabled": "true",
		"advisor.interval_seconds": "120",
		"alerting.enabled": "true",
		"alerting.slack_webhook_url": "https://hooks.slack.com/all",
		"alerting.check_interval_seconds": "60",
		"alerting.cooldown_minutes": "30",
		"alerting.quiet_hours_start": "23:00",
		"alerting.quiet_hours_end": "07:00",
		"retention.snapshots_days": "14",
		"retention.findings_days": "60",
		"retention.actions_days": "180",
		"retention.explains_days": "30"
	}`

	w := doRequestWithUser(
		putHandler, "PUT", "/api/v1/config/global",
		body, user)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	// Verify hot-reload applied all fields.
	if cfg.Collector.IntervalSeconds != 15 {
		t.Errorf("collector.interval: got %d, want 15",
			cfg.Collector.IntervalSeconds)
	}
	if cfg.Trust.Level != "autonomous" {
		t.Errorf("trust.level: got %q, want autonomous",
			cfg.Trust.Level)
	}
	if !cfg.LLM.Enabled {
		t.Error("llm.enabled: got false, want true")
	}
	if !cfg.Advisor.Enabled {
		t.Error("advisor.enabled: got false, want true")
	}
	if !cfg.Alerting.Enabled {
		t.Error("alerting.enabled: got false, want true")
	}

	// GET and verify all overridden keys are source=override.
	w = doRequest(
		getHandler, "GET", "/api/v1/config/global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status: got %d, want 200; body: %s",
			w.Code, w.Body.String())
	}

	var resp struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Verify a representative set of fields from each section.
	checks := map[string]struct {
		wantVal    any
		wantSource string
	}{
		"collector.interval_seconds": {
			float64(15), "override"},
		"collector.batch_size": {
			float64(500), "override"},
		"analyzer.slow_query_threshold_ms": {
			float64(200), "override"},
		"analyzer.unused_index_window_days": {
			float64(21), "override"},
		"trust.level": {
			"autonomous", "override"},
		"trust.tier3_safe": {
			true, "override"},
		"safety.cpu_ceiling_pct": {
			float64(95), "override"},
		"safety.lock_timeout_ms": {
			float64(2000), "override"},
		"llm.enabled": {
			true, "override"},
		"llm.model": {
			"gpt-4o", "override"},
		"llm.optimizer.enabled": {
			true, "override"},
		"advisor.enabled": {
			true, "override"},
		"advisor.interval_seconds": {
			float64(120), "override"},
		"alerting.enabled": {
			true, "override"},
		"alerting.check_interval_seconds": {
			float64(60), "override"},
		"retention.snapshots_days": {
			float64(14), "override"},
		"retention.explains_days": {
			float64(30), "override"},
	}

	for key, want := range checks {
		entry, ok := resp.Config[key]
		if !ok {
			t.Errorf("GET missing key: %s", key)
			continue
		}
		m, ok := entry.(map[string]any)
		if !ok {
			t.Errorf("%s: expected map, got %T", key, entry)
			continue
		}
		if m["value"] != want.wantVal {
			t.Errorf("%s value: got %v (%T), want %v",
				key, m["value"], m["value"], want.wantVal)
		}
		if m["source"] != want.wantSource {
			t.Errorf("%s source: got %v, want %s",
				key, m["source"], want.wantSource)
		}
	}
}
