package agenttools

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration"
)

// No concurrent access tests: reviewUnproven is a pure function of its
// arguments.

func classifyForLint(sql string) []migration.DDLClassification {
	return migration.NewRegexClassifier().Classify(sql, 170000)
}

func TestReviewUnprovenRaisesSafeToReview(t *testing.T) {
	sql := "ALTER TABLE app.t SET LOGGED"
	res := reviewUnproven(LintResult{Verdict: "safe"}, classifyForLint(sql), sql)
	if res.Verdict != "review" {
		t.Fatalf("verdict %q, want review", res.Verdict)
	}
	if !strings.Contains(res.Note, "1 statement") ||
		!strings.Contains(res.Note, "allowlist") {
		t.Fatalf("note %q must count the statement and name the allowlist", res.Note)
	}
	if res.MaxRiskScore != 0 {
		t.Fatalf("max score %v invented for an unscored statement", res.MaxRiskScore)
	}
}

func TestReviewUnprovenKeepsAllowlistedSafe(t *testing.T) {
	sql := "SET lock_timeout = '5s'; CREATE INDEX CONCURRENTLY i ON app.t (c); " +
		"ALTER TABLE app.t ADD COLUMN c int DEFAULT 0"
	in := LintResult{Verdict: "safe", Note: "kept"}
	res := reviewUnproven(in, classifyForLint(sql), sql)
	if res.Verdict != "safe" || res.Note != "kept" {
		t.Fatalf("result %+v, want the input unchanged", res)
	}
}

// A statement a real rule recognized is scored by its rule; only the
// statements nothing recognized are raised (the plan's scope).
func TestReviewUnprovenLeavesRuleScoredStatements(t *testing.T) {
	sql := "CREATE INDEX i ON app.t (c)"
	classified := classifyForLint(sql)
	if len(classified) == 0 {
		t.Fatal("the classifier no longer flags a plain CREATE INDEX")
	}
	res := reviewUnproven(LintResult{Verdict: "safe"}, classified, sql)
	if res.Verdict != "safe" || res.Note != "" {
		t.Fatalf("result %+v, want the rule's verdict unchanged", res)
	}
}

// ddl_missing_lock_timeout is advice about the batch, not a recognition
// of the statement's own hazard.
func TestReviewUnprovenIgnoresLockTimeoutOnlyMatch(t *testing.T) {
	sql := "ALTER TABLE app.t SET LOGGED"
	lt := migration.DDLClassification{RuleID: "ddl_missing_lock_timeout", Statement: sql}
	res := reviewUnproven(LintResult{Verdict: "safe"}, []migration.DDLClassification{lt}, sql)
	if res.Verdict != "review" {
		t.Fatalf("verdict %q, want review", res.Verdict)
	}
}

func TestReviewUnprovenNeverLowers(t *testing.T) {
	sql := "SELECT 1; ALTER TABLE app.t RENAME TO u"
	res := reviewUnproven(LintResult{Verdict: "risky", MaxRiskScore: 0.9, Note: "n"},
		nil, sql)
	if res.Verdict != "risky" || res.MaxRiskScore != 0.9 {
		t.Fatalf("result %+v, want risky kept", res)
	}
	if !strings.HasPrefix(res.Note, "n ") || !strings.Contains(res.Note, "2 statements") {
		t.Fatalf("note %q must keep the old note and count both statements", res.Note)
	}
}

func TestReviewUnprovenUntokenizable(t *testing.T) {
	sql := "CREATE TABLE app.t (a text DEFAULT 'x); ALTER TABLE app.big SET LOGGED"
	res := reviewUnproven(LintResult{Verdict: "safe"}, classifyForLint(sql), sql)
	if res.Verdict != "review" || !strings.Contains(res.Note, "tokenize") {
		t.Fatalf("result %+v, want review with a tokenize note", res)
	}
}

func TestReviewUnprovenEmptyInputs(t *testing.T) {
	res := reviewUnproven(LintResult{Verdict: "safe"}, nil, "")
	if res.Verdict != "safe" || res.Note != "" {
		t.Fatalf("result %+v, want unchanged for no statements", res)
	}
}
