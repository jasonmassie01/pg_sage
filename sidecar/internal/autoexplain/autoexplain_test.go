package autoexplain

import (
	"strings"
	"testing"
)

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
