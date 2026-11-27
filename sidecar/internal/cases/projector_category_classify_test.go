package cases_test

import (
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
)

// No concurrency test: projection is a pure function of its inputs.

func categoryFinding(category, objectType, object, sql string) cases.SourceFinding {
	return cases.SourceFinding{ID: "7", DatabaseName: "prod", Category: category,
		Severity: cases.SeverityWarning, ObjectType: objectType, ObjectIdentifier: object,
		Title: category, RecommendedSQL: sql}
}

func onlyCandidate(t *testing.T, f cases.SourceFinding) cases.ActionCandidate {
	t.Helper()
	c := cases.ProjectFinding(f, executor.ClassifySQL)
	if len(c.ActionCandidates) != 1 {
		t.Fatalf("%s %q: candidates = %+v, want one", f.Category, f.RecommendedSQL,
			c.ActionCandidates)
	}
	return c.ActionCandidates[0]
}

// The category paths (vacuum autopilot) carry the executor's contract for
// the finding's own statement, not the category's default type: the tuner
// emits an instance autovacuum setting as ALTER SYSTEM (cases-04).
func TestCategoryCandidatesTakeTheStatementsContract(t *testing.T) {
	tests := []struct {
		category, objectType, object, sql, want, risk string
	}{
		{"vacuum_tuning", "configuration", "instance:autovacuum_vacuum_scale_factor",
			"ALTER SYSTEM SET autovacuum_vacuum_scale_factor = '0.05'",
			"alter_system_guc", "moderate"},
		{"vacuum_tuning", "table", "public.orders",
			"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.05)",
			"set_table_autovacuum", "moderate"},
		{"table_bloat", "table", "public.orders", "VACUUM public.orders;",
			"vacuum_table", "safe"},
		{"table_bloat", "table", "public.orders", "VACUUM FULL public.orders",
			"ddl_preflight", "high"},
		{"schema_lint:lint_bloated_table", "table", "public.orders",
			"VACUUM (FULL) public.orders", "ddl_preflight", "high"},
		{"reindex_candidate", "index", "public.idx_orders_status",
			"REINDEX INDEX CONCURRENTLY public.idx_orders_status;",
			"reindex_concurrently", "moderate"},
		{"reindex_candidate", "index", "public.idx_orders_status",
			"REINDEX INDEX public.idx_orders_status", "ddl_preflight", "high"},
		{"reindex_candidate", "schema", "public", "REINDEX SCHEMA CONCURRENTLY public",
			"ddl_preflight", "high"},
	}
	for _, tt := range tests {
		got := onlyCandidate(t, categoryFinding(tt.category, tt.objectType, tt.object, tt.sql))
		if got.ActionType != tt.want || got.RiskTier != tt.risk {
			t.Errorf("%s %q = %s/%s, want %s/%s", tt.category, tt.sql, got.ActionType,
				got.RiskTier, tt.want, tt.risk)
		}
		if got.ProposedSQL != tt.sql {
			t.Errorf("%s: proposed_sql = %q, want %q", tt.category, got.ProposedSQL, tt.sql)
		}
		if tt.want == "ddl_preflight" && !slices.Equal(got.OutputModes,
			[]string{"generate_pr_or_script"}) {
			t.Errorf("%s %q output modes = %v, want script-only", tt.category, tt.sql,
				got.OutputModes)
		}
	}
}

// A retyped category candidate drops the default type's plan: an ALTER
// SYSTEM setting is not verified by reading reloptions.
func TestRetypedCategoryCandidateDropsTheDefaultPlan(t *testing.T) {
	got := onlyCandidate(t, categoryFinding("vacuum_tuning", "configuration",
		"instance:autovacuum_naptime", "ALTER SYSTEM SET autovacuum_naptime = '30s'"))
	if got.RecoveryDetail != "" {
		t.Errorf("recovery detail %q is the reloptions plan", got.RecoveryDetail)
	}
	for _, step := range got.VerificationPlan {
		if step == "verify reloptions contain expected autovacuum settings" {
			t.Errorf("verification plan %v is the reloptions plan", got.VerificationPlan)
		}
	}
	if got.ScriptOutput == nil || got.ScriptOutput.MigrationSQL != got.ProposedSQL {
		t.Errorf("script output = %+v, want the statement", got.ScriptOutput)
	}
}

// Invalid input: a category finding whose statement no contract runs (not
// the category's maintenance at all) is never shown as that maintenance,
// nor as executable: it is a high-risk, script-only preflight.
func TestCategoryStatementWithoutContractIsAPreflight(t *testing.T) {
	for _, tt := range []struct{ category, sql string }{
		{"table_bloat", "TRUNCATE public.orders"},
		{"table_bloat", "SELECT 1"},
		{"vacuum_tuning", "ALTER ROLE app SET autovacuum_naptime = '30s'"},
	} {
		got := onlyCandidate(t, categoryFinding(tt.category, "table", "public.orders", tt.sql))
		if got.ActionType != "ddl_preflight" || got.RiskTier != "high" ||
			slices.Contains(got.OutputModes, "queue_for_approval") ||
			slices.Contains(got.OutputModes, "execute") {
			t.Errorf("%s %q projected %s/%s %v", tt.category, tt.sql, got.ActionType,
				got.RiskTier, got.OutputModes)
		}
	}
}

// Nil classifier: a category statement fails closed to a preflight.
func TestCategoryCandidateWithoutClassifierFailsClosed(t *testing.T) {
	c := cases.ProjectFinding(categoryFinding("table_bloat", "table", "public.orders",
		"VACUUM public.orders"), nil)
	if len(c.ActionCandidates) != 1 || c.ActionCandidates[0].ActionType != "ddl_preflight" ||
		c.ActionCandidates[0].RiskTier != "high" {
		t.Fatalf("nil classifier table_bloat = %+v, want a high-risk preflight",
			c.ActionCandidates)
	}
}
