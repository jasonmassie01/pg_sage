package autoexplain

import (
	"testing"
)

func TestIsExplainable(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"select", "SELECT 1", true},
		{
			"select lower",
			"select * from users",
			true,
		},
		{
			"insert",
			"INSERT INTO foo VALUES (1)",
			true,
		},
		{
			"update",
			"UPDATE foo SET x=1",
			true,
		},
		{"delete", "DELETE FROM foo", true},
		{
			"with cte",
			"WITH cte AS (SELECT 1) SELECT * FROM cte",
			true,
		},
		{
			"leading whitespace",
			"  SELECT 1",
			true,
		},
		{
			"set",
			"SET search_path TO public",
			false,
		},
		{"vacuum", "VACUUM foo", false},
		{
			"create index",
			"CREATE INDEX idx ON foo (bar)",
			false,
		},
		{
			"copy",
			"COPY foo FROM stdin",
			false,
		},
		{"empty string", "", false},
		{"whitespace only", "   ", false},
		{
			"drop table",
			"DROP TABLE foo",
			false,
		},
		{
			"alter table",
			"ALTER TABLE foo ADD COLUMN bar int",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isExplainable(tt.query)
			if got != tt.want {
				t.Errorf(
					"isExplainable(%q) = %v, want %v",
					tt.query, got, tt.want,
				)
			}
		})
	}
}

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
