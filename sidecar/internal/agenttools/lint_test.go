package agenttools

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration"
)

// No concurrent access tests: reviewUnproven is a pure function of its
// arguments.

func stmtWith(status migration.StatementStatus, rules ...string) migration.ClassifiedStatement {
	st := migration.ClassifiedStatement{Statement: "stmt", Status: status}
	for _, r := range rules {
		st.Classifications = append(st.Classifications, migration.DDLClassification{RuleID: r})
	}
	return st
}

func classification(stmts ...migration.ClassifiedStatement) migration.Classification {
	return migration.Classification{Statements: stmts}
}

func TestReviewUnprovenRaisesSafeToReview(t *testing.T) {
	cls := classification(stmtWith(migration.StatementUnrecognized))
	res := reviewUnproven(LintResult{Verdict: "safe"}, cls)
	if res.Verdict != "review" {
		t.Fatalf("verdict %q, want review", res.Verdict)
	}
	if !strings.Contains(res.Note, "1 statement") ||
		!strings.Contains(res.Note, "known safe form") {
		t.Fatalf("note %q must count the statement and say it is not a known form", res.Note)
	}
	if res.MaxRiskScore != 0 {
		t.Fatalf("max score %v invented for an unscored statement", res.MaxRiskScore)
	}
}

// On a build without the SQL parser the note names the allowlist, the
// only proof of a safe form there.
func TestReviewUnprovenDegradedNamesTheAllowlist(t *testing.T) {
	cls := classification(stmtWith(migration.StatementUnrecognized))
	cls.Degraded = true
	res := reviewUnproven(LintResult{Verdict: "safe"}, cls)
	if res.Verdict != "review" || !strings.Contains(res.Note, "allowlist") {
		t.Fatalf("result %+v, want review naming the allowlist", res)
	}
}

func TestReviewUnprovenKeepsKnownSafe(t *testing.T) {
	cls := classification(stmtWith(migration.StatementKnown), stmtWith(migration.StatementKnown))
	in := LintResult{Verdict: "safe", Note: "kept"}
	if res := reviewUnproven(in, cls); res.Verdict != "safe" || res.Note != "kept" {
		t.Fatalf("result %+v, want the input unchanged", res)
	}
}

// A statement a rule recognized is known and scored by its rule.
func TestReviewUnprovenLeavesRuleScoredStatements(t *testing.T) {
	cls := classification(stmtWith(migration.StatementKnown, "ddl_index_not_concurrent"))
	res := reviewUnproven(LintResult{Verdict: "safe"}, cls)
	if res.Verdict != "safe" || res.Note != "" {
		t.Fatalf("result %+v, want the rule's verdict unchanged", res)
	}
}

// The status decides, not the rules: an unrecognized statement carrying
// only the lock_timeout advice is still raised.
func TestReviewUnprovenIgnoresLockTimeoutOnlyMatch(t *testing.T) {
	cls := classification(stmtWith(migration.StatementUnrecognized, "ddl_missing_lock_timeout"))
	if res := reviewUnproven(LintResult{Verdict: "safe"}, cls); res.Verdict != "review" {
		t.Fatalf("verdict %q, want review", res.Verdict)
	}
}

func TestReviewUnprovenNeverLowers(t *testing.T) {
	cls := classification(stmtWith(migration.StatementUnrecognized),
		stmtWith(migration.StatementUnrecognized), stmtWith(migration.StatementKnown))
	res := reviewUnproven(LintResult{Verdict: "risky", MaxRiskScore: 0.9, Note: "n"}, cls)
	if res.Verdict != "risky" || res.MaxRiskScore != 0.9 {
		t.Fatalf("result %+v, want risky kept", res)
	}
	if !strings.HasPrefix(res.Note, "n ") || !strings.Contains(res.Note, "2 statements") {
		t.Fatalf("note %q must keep the old note and count both statements", res.Note)
	}
}

func TestReviewUnprovenUntokenizable(t *testing.T) {
	cls := classification(stmtWith(migration.StatementKnown))
	cls.Untokenizable = true
	res := reviewUnproven(LintResult{Verdict: "safe"}, cls)
	if res.Verdict != "review" || !strings.Contains(res.Note, "tokenize") {
		t.Fatalf("result %+v, want review with a tokenize note", res)
	}
}

func TestReviewUnprovenEmptyInputs(t *testing.T) {
	res := reviewUnproven(LintResult{Verdict: "safe"}, migration.Classification{})
	if res.Verdict != "safe" || res.Note != "" {
		t.Fatalf("result %+v, want unchanged for no statements", res)
	}
}
