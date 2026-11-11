package autoexplain

import (
	"strings"
	"testing"
)

func TestDefaultSessionConfig(t *testing.T) {
	cfg := DefaultSessionConfig(500)

	if cfg.LogMinDurationMs != 500 {
		t.Errorf(
			"LogMinDurationMs = %d, want 500",
			cfg.LogMinDurationMs,
		)
	}
	if !cfg.LogAnalyze {
		t.Error("LogAnalyze should be true")
	}
	if !cfg.LogBuffers {
		t.Error("LogBuffers should be true")
	}
	if !cfg.LogNested {
		t.Error("LogNested should be true")
	}
}

func TestDefaultSessionConfig_ZeroThreshold(t *testing.T) {
	cfg := DefaultSessionConfig(0)
	if cfg.LogMinDurationMs != 0 {
		t.Errorf(
			"LogMinDurationMs = %d, want 0",
			cfg.LogMinDurationMs,
		)
	}
}

func TestAvailability_SharedPreload(t *testing.T) {
	a := Availability{
		SharedPreload: true,
		Available:     true,
		Method:        "shared_preload",
	}
	if !a.Available {
		t.Error("Available should be true")
	}
	if a.Method != "shared_preload" {
		t.Errorf("Method = %q, want shared_preload", a.Method)
	}
}

func TestAvailability_SessionLoad(t *testing.T) {
	a := Availability{
		SessionLoad: true,
		Available:   true,
		Method:      "session_load",
	}
	if !a.Available {
		t.Error("Available should be true")
	}
	if a.Method != "session_load" {
		t.Errorf("Method = %q, want session_load", a.Method)
	}
}

func TestAvailability_Unavailable(t *testing.T) {
	a := Availability{
		Available: false,
		Method:    "unavailable",
	}
	if a.Available {
		t.Error("Available should be false")
	}
	if a.Method != "unavailable" {
		t.Errorf("Method = %q, want unavailable", a.Method)
	}
	if a.SharedPreload {
		t.Error("SharedPreload should be false")
	}
	if a.SessionLoad {
		t.Error("SessionLoad should be false")
	}
}

func TestCollectorConfig_Defaults(t *testing.T) {
	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       10,
		LogMinDurationMs:       500,
		PreferSessionLoad:      false,
	}
	if cfg.CollectIntervalSeconds != 60 {
		t.Errorf(
			"CollectIntervalSeconds = %d, want 60",
			cfg.CollectIntervalSeconds,
		)
	}
	if cfg.MaxPlansPerCycle != 10 {
		t.Errorf(
			"MaxPlansPerCycle = %d, want 10",
			cfg.MaxPlansPerCycle,
		)
	}
	if cfg.LogMinDurationMs != 500 {
		t.Errorf(
			"LogMinDurationMs = %d, want 500",
			cfg.LogMinDurationMs,
		)
	}
}

func TestBuildSetStatements_AllEnabled(t *testing.T) {
	scfg := SessionConfig{
		LogMinDurationMs: 200,
		LogAnalyze:       true,
		LogBuffers:       true,
		LogNested:        true,
	}
	stmts := buildSetStatements(scfg)

	// 2 base + 3 optional = 5
	if len(stmts) != 5 {
		t.Fatalf("got %d statements, want 5", len(stmts))
	}
	if stmts[0] != "SET auto_explain.log_min_duration = '200ms'" {
		t.Errorf("stmts[0] = %q", stmts[0])
	}
	if stmts[1] != "SET auto_explain.log_format = 'json'" {
		t.Errorf("stmts[1] = %q", stmts[1])
	}
}

func TestBuildSetStatements_NoneEnabled(t *testing.T) {
	scfg := SessionConfig{
		LogMinDurationMs: 1000,
	}
	stmts := buildSetStatements(scfg)

	// Only the 2 base statements
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2", len(stmts))
	}
}

func TestExtractPlanMetrics_ValidJSON(t *testing.T) {
	planJSON := []byte(`[{
		"Plan": {"Total Cost": 42.5},
		"Execution Time": 1.23
	}]`)
	cost, execTime := extractPlanMetrics(planJSON)
	if cost != 42.5 {
		t.Errorf("total_cost = %f, want 42.5", cost)
	}
	if execTime != 1.23 {
		t.Errorf("execution_time = %f, want 1.23", execTime)
	}
}

func TestExtractPlanMetrics_InvalidJSON(t *testing.T) {
	cost, execTime := extractPlanMetrics([]byte("not json"))
	if cost != 0 || execTime != 0 {
		t.Errorf(
			"expected (0, 0), got (%f, %f)",
			cost, execTime,
		)
	}
}

func TestExtractPlanMetrics_EmptyArray(t *testing.T) {
	cost, execTime := extractPlanMetrics([]byte("[]"))
	if cost != 0 || execTime != 0 {
		t.Errorf(
			"expected (0, 0), got (%f, %f)",
			cost, execTime,
		)
	}
}

func TestEnableHint(t *testing.T) {
	tests := []struct {
		name        string
		platform    string
		wantContain string
		wantEmpty   bool
	}{
		{
			name:        "cloud-sql mentions cloudsql.enable_auto_explain flag",
			platform:    "cloud-sql",
			wantContain: "cloudsql.enable_auto_explain",
		},
		{
			name:        "cloud-sql mentions gcloud command",
			platform:    "cloud-sql",
			wantContain: "gcloud sql instances patch",
		},
		{
			name:        "alloydb mentions shared_preload_libraries flag",
			platform:    "alloydb",
			wantContain: "shared_preload_libraries",
		},
		{
			name:        "rds mentions parameter group",
			platform:    "rds",
			wantContain: "parameter group",
		},
		{
			name:        "aurora mentions parameter group",
			platform:    "aurora",
			wantContain: "parameter group",
		},
		{
			name:        "azure mentions azure.extensions",
			platform:    "azure",
			wantContain: "azure.extensions",
		},
		{
			name:      "self-managed returns empty",
			platform:  "self-managed",
			wantEmpty: true,
		},
		{
			name:      "empty platform returns empty",
			platform:  "",
			wantEmpty: true,
		},
		{
			name:      "unknown platform returns empty",
			platform:  "supabase",
			wantEmpty: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EnableHint(tc.platform)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("EnableHint(%q) = %q, want empty",
						tc.platform, got)
				}
				return
			}
			if got == "" {
				t.Errorf("EnableHint(%q) = empty, want non-empty",
					tc.platform)
				return
			}
			if !strings.Contains(got, tc.wantContain) {
				t.Errorf("EnableHint(%q) = %q, want substring %q",
					tc.platform, got, tc.wantContain)
			}
		})
	}
}
