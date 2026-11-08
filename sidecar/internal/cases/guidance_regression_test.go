package cases

import (
	"strings"
	"testing"
)

func TestUnprovenIndexIdentityDoesNotInventRecoveryOrVerification(t *testing.T) {
	for name, sql := range map[string]string{
		"unnamed foreign key index":       "CREATE INDEX CONCURRENTLY ON public.orders (customer_id);",
		"existing index may be untouched": "CREATE INDEX CONCURRENTLY IF NOT EXISTS orders_customer ON public.orders (customer_id);",
		"missing key definition":          "CREATE INDEX CONCURRENTLY orders_customer ON public.orders;",
		"multiple statements":             "CREATE INDEX CONCURRENTLY orders_customer ON public.orders (customer_id); SELECT 1;",
	} {
		t.Run(name, func(t *testing.T) {
			c := ProjectFinding(SourceFinding{Category: "missing_index", RecommendedSQL: sql})
			if len(c.ActionCandidates) != 1 {
				t.Fatalf("expected reviewable proposal: %#v", c)
			}
			a := c.ActionCandidates[0]
			if a.RollbackClass != "unknown" || a.RecoveryDetail == "" {
				t.Fatalf("unproven index must require identity capture: %#v", a)
			}
			if a.ScriptOutput == nil || a.ScriptOutput.RollbackSQL != "" {
				t.Fatalf("must not invent an inverse: %#v", a.ScriptOutput)
			}
			for _, query := range a.ScriptOutput.VerificationSQL {
				if !strings.HasPrefix(query, "--") {
					t.Fatalf("must not invent catalog verification: %s", query)
				}
			}
		})
	}
}

func TestQuotedIndexIdentitySurvivesRecoveryAndCatalogLiteral(t *testing.T) {
	c := ProjectFinding(SourceFinding{
		Category:       "missing_index",
		RecommendedSQL: `CREATE INDEX CONCURRENTLY "Customer's ""lookup""" ON "Sales'Q"."Orders" (customer_id);`,
	})
	a := c.ActionCandidates[0]
	if a.RollbackClass != "reversible" {
		t.Fatalf("named index has derived recovery: %#v", a)
	}
	if want := `DROP INDEX CONCURRENTLY "Sales'Q"."Customer's ""lookup""";`; a.ScriptOutput.RollbackSQL != want {
		t.Fatalf("rollback = %q, want %q", a.ScriptOutput.RollbackSQL, want)
	}
	if want := `SELECT indisvalid, indisready FROM pg_index WHERE indexrelid = '"Sales''Q"."Customer''s ""lookup"""'::regclass;`; a.ScriptOutput.VerificationSQL[0] != want {
		t.Fatalf("verification = %q, want %q", a.ScriptOutput.VerificationSQL[0], want)
	}
	for _, query := range a.ScriptOutput.VerificationSQL {
		if strings.HasPrefix(query, "EXPLAIN --") {
			t.Fatalf("placeholder must not become invalid SQL: %s", query)
		}
	}
}

func TestDropIndexRecoveryRequiresMatchingCapturedDefinition(t *testing.T) {
	for _, test := range []struct {
		name, rollback string
		valid          bool
	}{
		{"missing definition", "", false},
		{"reminder only", "-- Recreate from version control.", false},
		{"wrong index", "CREATE INDEX other ON public.orders (customer_id);", false},
		{"wrong schema", "CREATE INDEX orders_customer ON archive.orders (customer_id);", false},
		{"if not exists", "CREATE INDEX IF NOT EXISTS orders_customer ON public.orders (customer_id);", false},
		{"extra statement", "CREATE INDEX orders_customer ON public.orders (customer_id); SELECT 1;", false},
		{"captured definition", "CREATE INDEX orders_customer ON public.orders (customer_id);", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := ProjectFinding(SourceFinding{
				Category: "duplicate_index", ObjectIdentifier: "public.orders_customer",
				RecommendedSQL: "DROP INDEX CONCURRENTLY public.orders_customer;",
				RollbackSQL:    test.rollback,
			}).ActionCandidates[0]
			if test.valid {
				if a.RollbackClass != "reversible" || a.ScriptOutput.RollbackSQL != test.rollback {
					t.Fatalf("matching definition must be retained: %#v", a)
				}
			} else if a.RollbackClass != "unknown" || a.RecoveryDetail == "" || a.ScriptOutput.RollbackSQL != "" {
				t.Fatalf("missing or mismatched recovery must be withheld: %#v", a)
			}
		})
	}
}

func TestUrgencyUsesEvidenceRatherThanSeverity(t *testing.T) {
	for _, severity := range []Severity{SeverityInfo, SeverityWarning, SeverityCritical} {
		c := ProjectFinding(SourceFinding{Severity: severity})
		if c.WhyNow != "Urgency not assessed" {
			t.Fatalf("severity %s invented urgency: %q", severity, c.WhyNow)
		}
	}
	for _, test := range []struct {
		finding SourceFinding
		want    string
	}{
		{SourceFinding{Detail: map[string]any{"n_mod_since_analyze": 1200}}, "table changed since last analyze: 1200 rows"},
		{SourceFinding{Detail: map[string]any{"projected_at": "2026-11-01T00:00:00Z"}}, "forecast threshold projected at 2026-11-01T00:00:00Z"},
	} {
		if got := ProjectFinding(test.finding).WhyNow; got != test.want {
			t.Fatalf("measured rationale = %q, want %q", got, test.want)
		}
	}
	sequence := ProjectFinding(SourceFinding{Category: "sequence_exhaustion", Severity: SeverityCritical})
	if !strings.Contains(sequence.WhyNow, "time to exhaustion and application impact have not been established") {
		t.Fatalf("sequence urgency must disclose unknowns: %q", sequence.WhyNow)
	}
}

func TestSizePreflightDoesNotPassMissingMeasurement(t *testing.T) {
	for _, test := range []struct {
		size int64
		want string
	}{{0, "pass"}, {-1, "unknown"}, {1024, "pass"}, {11 * 1024 * 1024 * 1024, "warn"}} {
		r := ddlPreflightFromFinding(SourceFinding{Detail: map[string]any{"table_size_bytes": test.size}})
		for _, check := range r.Checks {
			if check.Name == "table_size" && check.Status != test.want {
				t.Fatalf("size %d: status = %s, want %s", test.size, check.Status, test.want)
			}
		}
	}
}

func TestPreflightMissingEvidenceIsUnknownAndMeasuredZeroIsRetained(t *testing.T) {
	missing := ddlPreflightFromFinding(SourceFinding{RecommendedSQL: "ALTER TABLE public.orders ADD COLUMN note text;"})
	for _, check := range missing.Checks {
		if check.Name != "lock_level" && check.Status != "unknown" {
			t.Fatalf("absent %s must not pass: %#v", check.Name, check)
		}
	}
	measured := ddlPreflightFromFinding(SourceFinding{Detail: map[string]any{
		"active_queries": 0, "pending_locks": 0, "replication_lag": 0.0,
		"table_size_bytes": int64(0), "requires_rewrite": false, "lock_timeout_ms": 500,
	}})
	for _, check := range measured.Checks {
		if check.Name != "lock_level" && check.Status != "pass" {
			t.Fatalf("measured zero/known evidence lost for %s: %#v", check.Name, check)
		}
	}
	for _, sql := range []string{
		"CREATE INDEX CONCURRENTLY ON public.orders (customer_id);",
		"REINDEX INDEX CONCURRENTLY public.orders_customer;",
	} {
		r := ddlPreflightFromFinding(SourceFinding{RecommendedSQL: sql})
		for _, check := range r.Checks {
			if (check.Name == "rewrite" || check.Name == "lock_level") && check.Status != "pass" {
				t.Fatalf("known concurrent semantics lost for %q: %#v", sql, check)
			}
		}
	}
}

func TestCommentOnlyRecoveryDoesNotBecomeAReversibilityGuarantee(t *testing.T) {
	for _, rollback := range []string{"", "-- Recreate later", "/* recreate later */", "/* outer /* nested */ comment */ ; -- later"} {
		c := NewCase(CaseInput{ActionCandidates: []ActionCandidate{{
			RollbackClass: "reversible",
			ScriptOutput:  &ScriptOutput{RollbackSQL: rollback, RiskLabels: []string{"moderate", "reversible"}, PRBody: "Rollback class: reversible."},
		}}})
		a := c.ActionCandidates[0]
		if a.RollbackClass != "unknown" || a.RecoveryDetail == "" || a.ScriptOutput.RollbackSQL != "" || strings.Contains(a.ScriptOutput.PRBody, "reversible") {
			t.Fatalf("comment-only recovery %q overclaimed: %#v", rollback, a)
		}
		for _, label := range a.ScriptOutput.RiskLabels {
			if label == "reversible" {
				t.Fatalf("risk labels retain false recovery claim: %#v", a.ScriptOutput)
			}
		}
	}
}

func TestRoleWorkMemDoesNotFabricateResetOfUncapturedPreviousValue(t *testing.T) {
	a := ProjectFinding(SourceFinding{Category: "query_work_mem_promotion", ObjectIdentifier: `reporting role`, RecommendedSQL: `ALTER ROLE "reporting role" SET work_mem = '64MB';`}).ActionCandidates[0]
	if a.RollbackClass != "unknown" || a.RecoveryDetail == "" || a.ScriptOutput.RollbackSQL != "" {
		t.Fatalf("role default requires captured prior state: %#v", a)
	}
	for _, label := range a.ScriptOutput.RiskLabels {
		if label == "reversible" {
			t.Fatalf("role default must not promise recovery: %#v", a.ScriptOutput)
		}
	}
}
