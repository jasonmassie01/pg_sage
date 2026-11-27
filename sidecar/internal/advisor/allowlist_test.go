package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// No concurrent access tests: applyConfigAllowlist is a pure transform of
// its argument slice.

func configFinding(sql string) analyzer.Finding {
	return analyzer.Finding{
		Category: "memory_tuning", Severity: "warning", ObjectType: "configuration",
		ObjectIdentifier: "instance", Title: "t", Detail: map[string]any{"k": "v"},
		Recommendation: "because", RecommendedSQL: sql, ActionRisk: "moderate",
	}
}

func assertAdvisory(t *testing.T, f analyzer.Finding, mention string) {
	t.Helper()
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Errorf("advisory finding kept SQL %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	if f.Severity != "info" || f.ActionRisk != "" {
		t.Errorf("advisory severity/risk = %q/%q, want info/\"\"", f.Severity, f.ActionRisk)
	}
	if !strings.Contains(f.Recommendation, "because") {
		t.Errorf("advisory dropped the rationale: %q", f.Recommendation)
	}
	if !strings.Contains(f.Recommendation, mention) {
		t.Errorf("advisory note %q does not mention %q", f.Recommendation, mention)
	}
}

func TestApplyConfigAllowlist_UnknownGUCBecomesAdvisory(t *testing.T) {
	for _, sql := range []string{
		"ALTER SYSTEM SET log_statement = 'all';",
		// idle_in_transaction_session_timeout is documented since advisors-07
		// (database scope, approval only); statement_timeout is not.
		"ALTER SYSTEM SET statement_timeout = '60s';",
		"ALTER SYSTEM RESET fsync;",
	} {
		got := applyConfigAllowlist([]analyzer.Finding{configFinding(sql)})
		if len(got) != 1 {
			t.Fatalf("%q: got %d findings, want 1", sql, len(got))
		}
		stmtName := strings.Fields(strings.TrimSuffix(sql, ";"))[3]
		assertAdvisory(t, got[0], stmtName)
	}
}

func TestApplyConfigAllowlist_AutonomousGUCUnchanged(t *testing.T) {
	in := configFinding("ALTER SYSTEM SET work_mem = '64MB';")
	got := applyConfigAllowlist([]analyzer.Finding{in})
	if len(got) != 1 || got[0].RecommendedSQL != in.RecommendedSQL ||
		got[0].Severity != "warning" || got[0].ActionRisk != "moderate" {
		t.Fatalf("autonomous GUC changed: %+v", got)
	}
	if _, marked := got[0].Detail[analyzer.DetailApprovalRequired]; marked {
		t.Error("reload-only allowlisted GUC must not require approval")
	}
}

func TestApplyConfigAllowlist_RestartGUCNeedsApproval(t *testing.T) {
	in := configFinding("ALTER SYSTEM SET shared_buffers = '2GB';")
	got := applyConfigAllowlist([]analyzer.Finding{in})
	if len(got) != 1 || got[0].RecommendedSQL != in.RecommendedSQL {
		t.Fatalf("restart GUC must stay executable for an operator: %+v", got)
	}
	reason, _ := got[0].Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(reason, "restart") {
		t.Errorf("approval reason = %q, want it to explain the restart", reason)
	}
	if in.Detail[analyzer.DetailApprovalRequired] != nil {
		t.Error("input finding's Detail map was mutated")
	}
}

func TestApplyConfigAllowlist_DisabledAutovacuumIsAdvisory(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE public.orders SET (autovacuum_enabled = false);`,
		`ALTER TABLE public.orders SET (toast.autovacuum_enabled = off);`,
		`ALTER TABLE public.orders SET (autovacuum_vacuum_threshold = 50, ` +
			`autovacuum_enabled = false);`,
	} {
		got := applyConfigAllowlist([]analyzer.Finding{configFinding(sql)})
		if len(got) != 1 {
			t.Fatalf("%q: got %d findings, want 1 advisory", sql, len(got))
		}
		assertAdvisory(t, got[0], "autovacuum_enabled")
	}
}

func TestApplyConfigAllowlist_UnknownReloptionIsAdvisory(t *testing.T) {
	got := applyConfigAllowlist([]analyzer.Finding{
		configFinding(`ALTER TABLE public.orders SET (parallel_workers = 8);`)})
	if len(got) != 1 {
		t.Fatalf("got %d findings", len(got))
	}
	assertAdvisory(t, got[0], "parallel_workers")
}

func TestApplyConfigAllowlist_SplitsMultiKeyReloptions(t *testing.T) {
	in := configFinding(`ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor` +
		` = 0.02, autovacuum_vacuum_threshold = 1000);`)
	in.ObjectIdentifier = "public.orders"
	got := applyConfigAllowlist([]analyzer.Finding{in})
	if len(got) != 2 {
		t.Fatalf("got %d findings, want one per reloption: %+v", len(got), got)
	}
	want := []string{
		`ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.02);`,
		`ALTER TABLE "public"."orders" SET (autovacuum_vacuum_threshold = 1000);`,
	}
	seen := map[string]bool{}
	for i, f := range got {
		if f.RecommendedSQL != want[i] {
			t.Errorf("split[%d] SQL = %q, want %q", i, f.RecommendedSQL, want[i])
		}
		if seen[f.ObjectIdentifier] || f.ObjectIdentifier == "public.orders" {
			t.Errorf("split[%d] object id %q is not distinct", i, f.ObjectIdentifier)
		}
		seen[f.ObjectIdentifier] = true
	}
}

func TestApplyConfigAllowlist_SingleReloptionAndOtherSQLUnchanged(t *testing.T) {
	in := []analyzer.Finding{
		configFinding(`ALTER TABLE public.orders SET (fillfactor = 90);`),
		bloatFinding(`VACUUM public.orders;`),
		configFinding(``),
		configFinding(`ALTER TABLE public.orders RESET (autovacuum_vacuum_threshold);`),
	}
	got := applyConfigAllowlist(in)
	if len(got) != len(in) {
		t.Fatalf("got %d findings, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i].RecommendedSQL != in[i].RecommendedSQL || got[i].Severity != "warning" {
			t.Errorf("finding %d changed: %+v", i, got[i])
		}
	}
}

func TestApplyConfigAllowlist_NilAndEmpty(t *testing.T) {
	if got := applyConfigAllowlist(nil); len(got) != 0 {
		t.Errorf("nil input gave %d findings", len(got))
	}
	if got := applyConfigAllowlist([]analyzer.Finding{}); len(got) != 0 {
		t.Errorf("empty input gave %d findings", len(got))
	}
}

// End to end through the LLM parse path: an unknown GUC survives as an
// advisory finding without SQL, and disabling autovacuum never executes.
func TestParseLLMFindings_AppliesConfigAllowlist(t *testing.T) {
	raw := `[{"object_identifier":"instance","severity":"warning","rationale":"log more",` +
		`"recommended_sql":"ALTER SYSTEM SET log_min_messages = 'debug5';"},` +
		`{"object_identifier":"public.t","severity":"warning","rationale":"stop vacuum",` +
		`"recommended_sql":"ALTER TABLE public.t SET (autovacuum_enabled = false);"},` +
		`{"object_identifier":"instance","severity":"warning","rationale":"spills",` +
		`"recommended_sql":"ALTER SYSTEM SET work_mem = '64MB';"}]`
	got := parseLLMFindings(raw, "memory_tuning", noopLog)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(got), got)
	}
	if got[0].RecommendedSQL != "" || got[1].RecommendedSQL != "" {
		t.Errorf("non-allowlisted changes kept SQL: %q / %q",
			got[0].RecommendedSQL, got[1].RecommendedSQL)
	}
	if got[2].RecommendedSQL != "ALTER SYSTEM SET work_mem = '64MB';" {
		t.Errorf("allowlisted change lost its SQL: %q", got[2].RecommendedSQL)
	}
}

func bloatFinding(sql string) analyzer.Finding {
	f := configFinding(sql)
	f.Category = "bloat_remediation"
	return f
}

// Defence in depth (advisors-04): a config sub-advisor's value is the
// setting change itself, so any other SQL it returns is advisory only.
func TestApplyConfigAllowlist_ConfigCategoryOtherSQLIsAdvisory(t *testing.T) {
	sqls := []string{
		"CREATE INDEX CONCURRENTLY i ON public.t (a);",
		"VACUUM FULL t;",
		"SELECT pg_terminate_backend(42);",
		"ALTER SYSTEM RESET ALL;",
		"ALTER DATABASE orders SET work_mem = '1GB';",
		"ALTER ROLE app SET work_mem = '1GB';",
		"DROP TABLE t;",
		"ALTER TABLE t ADD COLUMN c int;",
	}
	for c := range configCategories {
		for _, sql := range sqls {
			f := configFinding(sql)
			f.Category = c
			got := applyConfigAllowlist([]analyzer.Finding{f})
			if len(got) != 1 {
				t.Fatalf("%s %q: got %d findings, want 1", c, sql, len(got))
			}
			assertAdvisory(t, got[0], "not a setting change")
		}
	}
}

// Boundary: the statements a config sub-advisor may run stay executable,
// a config finding that carries no SQL is left alone, and the advisory
// categories keep their own handling.
func TestApplyConfigAllowlist_ConfigCategoryBoundary(t *testing.T) {
	for _, sql := range []string{
		"ALTER SYSTEM SET work_mem = '64MB';",
		"ALTER SYSTEM RESET work_mem;",
		"ALTER TABLE public.t SET (fillfactor = 90);",
		"ALTER TABLE public.t RESET (fillfactor);",
	} {
		f := configFinding(sql)
		f.Category = "wal_tuning"
		got := applyConfigAllowlist([]analyzer.Finding{f})
		if len(got) != 1 || got[0].RecommendedSQL != sql || got[0].ActionRisk != "moderate" {
			t.Errorf("%q: got %+v, want unchanged", sql, got)
		}
	}
	empty := applyConfigAllowlist([]analyzer.Finding{configFinding("")})
	if len(empty) != 1 || empty[0].Recommendation != "because" ||
		empty[0].Severity != "warning" {
		t.Errorf("SQL-less config finding changed: %+v", empty)
	}
	for _, c := range []string{"bloat_remediation", "query_rewrite"} {
		f := configFinding("VACUUM FULL t;")
		f.Category = c
		got := applyConfigAllowlist([]analyzer.Finding{f})
		if len(got) != 1 || got[0].RecommendedSQL != "VACUUM FULL t;" {
			t.Errorf("%s: got %+v, want its own handling unchanged", c, got)
		}
	}
}

// Through the parse path the WAL advisor's CREATE INDEX or VACUUM FULL is
// kept as advice with no SQL and no action risk (it was "safe" before).
func TestParseLLMFindings_WALAdvisorNonConfigSQLIsAdvisory(t *testing.T) {
	for _, sql := range []string{"CREATE INDEX CONCURRENTLY i ON t (a)", "VACUUM FULL t"} {
		raw := `[{"object_identifier":"instance","severity":"warning",` +
			`"rationale":"because","recommended_sql":"` + sql + `"}]`
		got := parseLLMFindings(raw, "wal_tuning", noopLog)
		if len(got) != 1 {
			t.Fatalf("%q: got %d findings, want 1", sql, len(got))
		}
		assertAdvisory(t, got[0], "not a setting change")
	}
}
