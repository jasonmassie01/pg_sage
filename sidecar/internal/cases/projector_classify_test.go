package cases_test

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// No concurrency test: projection is a pure function of its inputs.

// executorSQLCorpus covers every statement form action_classify.go names
// (CHECK-06), plus the boundaries cases-04 calls out.
func executorSQLCorpus() []string {
	replace := optimizer.IndexReplaceSQL("CREATE INDEX CONCURRENTLY orders_status_created "+
		"ON public.orders (status, created_at)", "public.orders_status_idx")
	return []string{
		replace,
		"ANALYZE public.orders",
		"CREATE INDEX CONCURRENTLY idx_orders_c ON public.orders (c)",
		"CREATE UNIQUE INDEX CONCURRENTLY idx_orders_u ON public.orders (u)",
		"DROP INDEX CONCURRENTLY public.idx_orders_c",
		"REINDEX INDEX CONCURRENTLY public.idx_orders_c",
		"REINDEX TABLE CONCURRENTLY public.orders",
		"VACUUM public.orders",
		"VACUUM (ANALYZE) public.orders",
		"SELECT pg_cancel_backend(4242)",
		"SELECT pg_terminate_backend(4242)",
		"ALTER SYSTEM SET wal_compression = on",
		"ALTER SYSTEM RESET work_mem",
		`ALTER DATABASE "prod" SET work_mem = '64MB'`,
		`ALTER DATABASE "prod" SET search_path = app`,
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.05)",
		"ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric",
		"INSERT INTO hint_plan.hints(query_id, hints) VALUES (777, 'Set(work_mem \"128MB\")')",
		"DELETE FROM hint_plan.hints WHERE query_id = 777",
		"CREATE STATISTICS public.sage_stx_orders_ab (dependencies) ON a, b FROM public.orders",
		"DROP STATISTICS IF EXISTS public.sage_stx_orders_ab",
		"VACUUM FULL public.orders",
		"VACUUM (FULL, ANALYZE) public.orders",
		"VACUUM",
		"DROP INDEX public.idx_orders_c",
		"REINDEX INDEX public.idx_orders_c",
		"CREATE INDEX idx_orders_c ON public.orders (c)",
		"ALTER ROLE app SET work_mem = '64MB'",
		"TRUNCATE public.orders",
	}
}

func parityFinding(sql string) cases.SourceFinding {
	return cases.SourceFinding{ID: "1", DatabaseName: "prod", Category: "sql_parity",
		Severity: cases.SeverityWarning, ObjectType: "table", ObjectIdentifier: "public.orders",
		Title: "parity", RecommendedSQL: sql}
}

// unclassifiedChange is a statement the executor runs under no contract
// that still changes the database: Cases shows it as a high-risk,
// script-only ddl_preflight.
func unclassifiedChange(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	for _, prefix := range []string{"CREATE ", "ALTER ", "DROP ", "GRANT ", "REVOKE ",
		"COMMENT ", "TRUNCATE ", "VACUUM", "REINDEX "} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// CHECK-06: every projected action type is the executor's own, and its
// risk tier is that contract's base tier.
func TestProjectedActionTypeMatchesExecutorClassifier(t *testing.T) {
	for _, sql := range executorSQLCorpus() {
		got := cases.ProjectFinding(parityFinding(sql), executor.ClassifySQL)
		want := executor.ActionTypeForSQL(sql)
		if want == "" && unclassifiedChange(sql) {
			want = "ddl_preflight"
		}
		if want == "" {
			if len(got.ActionCandidates) != 0 {
				t.Errorf("%q: candidates %+v, want none", sql, got.ActionCandidates)
			}
			continue
		}
		if len(got.ActionCandidates) != 1 {
			t.Errorf("%q: %d candidates, want 1 (%s)", sql, len(got.ActionCandidates), want)
			continue
		}
		candidate := got.ActionCandidates[0]
		if candidate.ActionType != want {
			t.Errorf("%q: action_type = %q, want the executor's %q", sql,
				candidate.ActionType, want)
		}
		contract, ok := executor.ContractForActionType(want)
		if !ok {
			t.Fatalf("%q: no contract for %q", sql, want)
		}
		if candidate.RiskTier != contract.BaseRiskTier {
			t.Errorf("%q: risk_tier = %q, want the contract's %q", sql,
				candidate.RiskTier, contract.BaseRiskTier)
		}
		if candidate.ProposedSQL != sql {
			t.Errorf("%q: proposed_sql = %q", sql, candidate.ProposedSQL)
		}
	}
}

// The advisor's config statements carry the GUC contracts, not a
// script-only preflight.
func TestConfigStatementsProjectGUCContracts(t *testing.T) {
	tests := []struct{ sql, want, risk string }{
		{"ALTER SYSTEM SET wal_compression = on;", "alter_system_guc", "moderate"},
		{`ALTER DATABASE "prod" SET work_mem = '64MB'`, "alter_database_guc", "moderate"},
		{`ALTER DATABASE "prod" SET search_path = app`, "ddl_preflight", "high"},
		{"CREATE UNIQUE INDEX CONCURRENTLY u ON public.orders (u)",
			"create_index_concurrently", "moderate"},
	}
	for _, tt := range tests {
		c := cases.ProjectFinding(parityFinding(tt.sql), executor.ClassifySQL)
		if len(c.ActionCandidates) != 1 {
			t.Fatalf("%q: candidates = %+v", tt.sql, c.ActionCandidates)
		}
		got := c.ActionCandidates[0]
		if got.ActionType != tt.want || got.RiskTier != tt.risk {
			t.Errorf("%q = %s/%s, want %s/%s", tt.sql, got.ActionType, got.RiskTier,
				tt.want, tt.risk)
		}
	}
}

// Invalid input: VACUUM FULL rewrites the table under an exclusive lock and
// a plain DROP INDEX blocks writes; neither is ever shown as the
// executor's safe or concurrent action.
func TestHeavyMaintenanceIsNeverShownAsSafe(t *testing.T) {
	for _, sql := range []string{"VACUUM FULL t", "VACUUM (FULL) public.t",
		"DROP INDEX i", "DROP INDEX public.i"} {
		c := cases.ProjectFinding(parityFinding(sql), executor.ClassifySQL)
		if len(c.ActionCandidates) != 1 {
			t.Fatalf("%q: candidates = %+v, want one script-only preflight", sql,
				c.ActionCandidates)
		}
		got := c.ActionCandidates[0]
		if got.RiskTier == "safe" || got.ActionType == "vacuum_table" ||
			got.ActionType == "drop_unused_index" {
			t.Errorf("%q projected %s/%s", sql, got.ActionType, got.RiskTier)
		}
		if got.ActionType != "ddl_preflight" || got.RiskTier != "high" {
			t.Errorf("%q = %s/%s, want ddl_preflight/high", sql, got.ActionType, got.RiskTier)
		}
		for _, mode := range got.OutputModes {
			if mode == "queue_for_approval" {
				t.Errorf("%q offers queue_for_approval; a preflight is script-only", sql)
			}
		}
	}
}

// Nil/empty: without a classifier nothing is executor-classified, so a
// change is a script-only preflight (fail closed) and a non-change has no
// candidate; blank SQL has no candidate and no risk tier.
func TestProjectionWithoutClassifierFailsClosed(t *testing.T) {
	c := cases.ProjectFinding(parityFinding("VACUUM public.orders"), nil)
	if len(c.ActionCandidates) != 1 || c.ActionCandidates[0].ActionType != "ddl_preflight" ||
		c.ActionCandidates[0].RiskTier != "high" {
		t.Fatalf("nil classifier VACUUM = %+v, want a high-risk preflight", c.ActionCandidates)
	}
	if c := cases.ProjectFinding(parityFinding("ANALYZE public.orders"), nil); len(
		c.ActionCandidates) != 0 {
		t.Fatalf("nil classifier ANALYZE = %+v, want no candidate", c.ActionCandidates)
	}
	if c := cases.ProjectFinding(parityFinding("  "), executor.ClassifySQL); len(
		c.ActionCandidates) != 0 {
		t.Fatalf("blank SQL = %+v, want no candidate", c.ActionCandidates)
	}
	if got := cases.RiskTierForSQL("", executor.ClassifySQL); got != "" {
		t.Fatalf("RiskTierForSQL(blank) = %q", got)
	}
	if got := cases.RiskTierForSQL("ALTER SYSTEM SET work_mem = '64MB'", nil); got != "high" {
		t.Fatalf("RiskTierForSQL without classifier = %q, want high", got)
	}
}

// The briefing's tier is the candidate's tier for the same statement.
func TestRiskTierForSQLIsTheExecutorContractTier(t *testing.T) {
	for _, sql := range executorSQLCorpus() {
		c := cases.ProjectFinding(parityFinding(sql), executor.ClassifySQL)
		got := cases.RiskTierForSQL(sql, executor.ClassifySQL)
		if len(c.ActionCandidates) == 0 {
			if got != "" && got != "high" {
				t.Errorf("%q: briefing tier %q for a statement with no candidate", sql, got)
			}
			continue
		}
		if got != c.ActionCandidates[0].RiskTier {
			t.Errorf("%q: briefing tier %q, candidate tier %q", sql, got,
				c.ActionCandidates[0].RiskTier)
		}
	}
}
