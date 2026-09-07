package executor

import "testing"

func TestConfigParamFromSQL(t *testing.T) {
	cases := map[string]string{
		"ALTER SYSTEM SET work_mem = '16MB';":            "work_mem",
		"ALTER SYSTEM SET max_connections = 100":         "max_connections",
		"alter system set random_page_cost = 1.1;":       "random_page_cost",
		"ALTER SYSTEM RESET shared_buffers;":             "shared_buffers",
		"CREATE INDEX ON t (a);":                         "",
	}
	for sql, want := range cases {
		if got := configParamFromSQL(sql); got != want {
			t.Errorf("configParamFromSQL(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestIsManagedProvider(t *testing.T) {
	managed := []string{"rds", "aurora", "cloud-sql", "cloudsql", "alloydb", "azure", "AWS"}
	for _, p := range managed {
		if !isManagedProvider(p) {
			t.Errorf("isManagedProvider(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"self-managed", "postgres", "", "ec2"} {
		if isManagedProvider(p) {
			t.Errorf("isManagedProvider(%q) = true, want false", p)
		}
	}
}

func TestRestartRequiredClassification(t *testing.T) {
	if !restartRequiredParams["shared_buffers"] || !restartRequiredParams["max_connections"] {
		t.Error("shared_buffers/max_connections must be restart-required")
	}
	if restartRequiredParams["work_mem"] || restartRequiredParams["random_page_cost"] {
		t.Error("work_mem/random_page_cost are reload-only, not restart-required")
	}
}

func TestOutcomeStatus(t *testing.T) {
	if outcomeStatus(true) != "success" {
		t.Error("in-effect should be success")
	}
	if outcomeStatus(false) != "applied_pending_restart" {
		t.Error("not-in-effect should be applied_pending_restart")
	}
}
