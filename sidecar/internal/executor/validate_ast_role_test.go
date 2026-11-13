package executor

import "testing"

// The parse-tree layer's CONNECTION LIMIT allowlist is the catalog-free
// half of the role budget rule (budget.RefusedRole): it runs in every
// build, so it is tested without cgo too (env-cgo).

// No concurrency or state-transition tests: the rule is a pure function.

func TestExecutorASTRulesRoleConnectionLimit(t *testing.T) {
	allowed := executorASTRules.RoleConnectionLimit
	if allowed == nil {
		t.Fatal("executorASTRules.RoleConnectionLimit is nil: " +
			"ALTER ROLE ... CONNECTION LIMIT has no allowlist")
	}
	for role, want := range map[string]bool{
		"app":              true,
		"budget_exec_app":  true,
		"App":              true,
		"postgresql":       true,
		"postgres":         false,
		"pg_monitor":       false,
		"pg_read_all_data": false,
		"pg_":              false,
		"cloudsqladmin":    false,
		"rdsadmin":         false,
		"rds_superuser":    false,
		"azure_superuser":  false,
		"alloydbadmin":     false,
		"":                 false,
	} {
		if got := allowed(role); got != want {
			t.Errorf("RoleConnectionLimit(%q) = %v, want %v", role, got, want)
		}
	}
}
