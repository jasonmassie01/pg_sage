package cases

import "testing"

// migration-01: the preflight's fallback lock table is the migration rule
// catalog, not a second table keyed on rule-name substrings.

func TestLockLevelForRule_FromRuleCatalog(t *testing.T) {
	tests := []struct {
		rule, sql, want string
	}{
		{"ddl_constraint_not_valid", "ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0)",
			"ACCESS EXCLUSIVE"},
		{"ddl_fk_not_valid", "ALTER TABLE t ADD CONSTRAINT f FOREIGN KEY (a) REFERENCES p",
			"SHARE ROW EXCLUSIVE"},
		{"ddl_vacuum_full", "", "ACCESS EXCLUSIVE"},
		{"ddl_cluster", "", "ACCESS EXCLUSIVE"},
		{"ddl_refresh_not_concurrent", "", "ACCESS EXCLUSIVE"},
		{"ddl_set_tablespace", "", "ACCESS EXCLUSIVE"},
		{"ddl_index_not_concurrent", "", "SHARE"},
		// The candidate's own concurrent SQL takes its own lock.
		{"ddl_index_not_concurrent", "CREATE INDEX CONCURRENTLY i ON t (a)",
			"SHARE UPDATE EXCLUSIVE"},
		{"ddl_reindex_not_concurrent", "REINDEX INDEX CONCURRENTLY i",
			"SHARE UPDATE EXCLUSIVE"},
		// Not a catalog rule: only an ALTER TABLE is known to need
		// ACCESS EXCLUSIVE; anything else stays unassessed.
		{"some_other_rule", "ALTER TABLE t ALTER c SET STATISTICS 100", "ACCESS EXCLUSIVE"},
		{"some_other_rule", "ANALYZE t", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := lockLevelForRule(tc.rule, tc.sql); got != tc.want {
			t.Errorf("lockLevelForRule(%q, %q) = %q, want %q", tc.rule, tc.sql, got, tc.want)
		}
	}
}

func TestPreflight_RewriteFromRuleCatalog(t *testing.T) {
	tests := []struct {
		rule        string
		wantRewrite bool
		wantStatus  string
	}{
		{"ddl_vacuum_full", true, "warn"},
		{"ddl_cluster", true, "warn"},
		{"ddl_add_column_volatile_default", true, "warn"},
		{"ddl_alter_type_rewrite", true, "warn"},
		{"ddl_set_not_null", false, "pass"},
		// Unknown rule: rewrite stays unassessed even with "rewrite" in it.
		{"custom_rewrite_check", false, "unknown"},
	}
	for _, tc := range tests {
		report := ddlPreflightFromFinding(SourceFinding{
			Category: "migration_safety", RuleID: tc.rule,
			Detail: map[string]any{"rule_id": tc.rule},
		})
		if report.RequiresRewrite != tc.wantRewrite {
			t.Errorf("%s: RequiresRewrite = %v, want %v", tc.rule,
				report.RequiresRewrite, tc.wantRewrite)
		}
		if got := checkByName(report, "rewrite").Status; got != tc.wantStatus {
			t.Errorf("%s: rewrite check = %q, want %q", tc.rule, got, tc.wantStatus)
		}
	}
}

// The detail's measured lock level and rewrite win over the catalog: an
// ALTER TYPE the advisor found binary-coercible does not rewrite.
func TestPreflight_DetailLockAndRewriteWin(t *testing.T) {
	report := ddlPreflightFromFinding(SourceFinding{
		Category: "migration_safety", RuleID: "ddl_alter_type_rewrite",
		Detail: map[string]any{"rule_id": "ddl_alter_type_rewrite",
			"lock_level": "ACCESS EXCLUSIVE", "requires_rewrite": false},
	})
	if report.LockLevel != "ACCESS EXCLUSIVE" || report.RequiresRewrite {
		t.Fatalf("report lock=%q rewrite=%v, want detail values", report.LockLevel,
			report.RequiresRewrite)
	}
	lock := checkByName(report, "lock_level")
	if lock.Status != "pass" || lock.Detail != "ACCESS EXCLUSIVE (classified at detection)" {
		t.Fatalf("lock_level check = %+v", lock)
	}
}

func checkByName(report *DDLPreflightReport, name string) PreflightCheck {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return PreflightCheck{}
}
