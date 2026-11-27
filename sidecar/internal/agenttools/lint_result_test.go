package agenttools

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/migration"
)

// No concurrent access tests: lintResultOf is a pure mapping.

// The report keeps lint_migration's shape: one item per hazard, and the
// lock_timeout rule listed as an unscored annotation.
func TestLintResultOfKeepsTheEstablishedShape(t *testing.T) {
	rep := migration.Report{Verdict: migration.VerdictRisky, MaxRiskScore: 0.8,
		PGVersion: 170000, RowThreshold: 500, Notes: []string{"a.", "b."},
		Statements: []migration.StatementReview{{Index: 2, Statement: "ALTER TABLE t ...",
			GuardedRewrite: "SET lock_timeout = '5s';\nALTER TABLE t ...;",
			Hazards: []migration.Hazard{{RuleID: "ddl_set_not_null", LockLevel: "ACCESS EXCLUSIVE",
				Table: "app.t", LiveRows: 42, TableSizeBytes: 4200, RiskScore: 0.8,
				Verdict: migration.VerdictRisky}},
			Annotations: []migration.Annotation{{RuleID: "ddl_missing_lock_timeout",
				Table: "app.t", Description: "d", SafeAlternative: "s"}}}}}
	res := lintResultOf(rep)
	if res.Verdict != "risky" || res.MaxRiskScore != 0.8 || res.Note != "a. b." ||
		res.PGVersion != 170000 || res.RowThreshold != 500 || len(res.Statements) != 2 {
		t.Fatalf("result %+v", res)
	}
	h, a := res.Statements[0], res.Statements[1]
	if h.RuleID != "ddl_set_not_null" || h.EstimatedRows != 42 || h.TableSizeBytes != 4200 ||
		h.StatementIndex != 2 || h.Verdict != "risky" || h.GuardedRewrite == "" || h.Annotation {
		t.Fatalf("hazard item %+v", h)
	}
	if a.RuleID != "ddl_missing_lock_timeout" || !a.Annotation || a.RiskScore != 0 ||
		a.SafeAlternative != "s" || a.Table != "app.t" {
		t.Fatalf("annotation item %+v", a)
	}
	if empty := lintResultOf(migration.Report{Verdict: "safe"}); empty.Statements == nil {
		t.Fatal("statements must be an empty list, not null")
	}
}
