package impact

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// withoutParser runs the engine as a build without libpg_query does.
func withoutParser(t *testing.T) {
	t.Helper()
	prev := astAvailable
	astAvailable = func() bool { return false }
	t.Cleanup(func() { astAvailable = prev })
}

// CR-18 in a build without the parser: the regex classifier scores the
// DDL (CR-01) and the review is never safe.
func TestTextModeIsNeverSafe(t *testing.T) {
	e := newEnv(t)
	withoutParser(t)
	a := e.analyze("ALTER TABLE rv.rv_orders ALTER COLUMN status TYPE varchar(20);\n" +
		"ALTER TABLE rv.rv_orders ADD COLUMN note text")
	if a.ParseMode != changereview.ParseText {
		t.Fatalf("parse mode %s, want text", a.ParseMode)
	}
	f := one(t, a, changereview.RuleTextMode)
	if f.Severity != changereview.SeverityWarning || !strings.Contains(f.Text, "text mode") {
		t.Fatalf("CR-18 %+v, want a text-mode warning", f)
	}
	var rewrite bool
	for _, cr01 := range byRule(a, RuleLockAndRewrite) {
		for _, ev := range citedOfKind(t, a, cr01, "risk") {
			rewrite = rewrite || decode[riskData](t, ev).RequiresRewrite
		}
	}
	if !rewrite {
		t.Fatalf("the classifier's rewrite of the type change is missing: %s", describe(a))
	}
	if verdict(a) == changereview.VerdictSafe {
		t.Fatal("text mode must never be safe")
	}
	b := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN note text")
	if verdict(b) != changereview.VerdictReview || len(b.Statements) == 0 {
		t.Fatalf("a harmless change in text mode: verdict %s, %d statements, want review",
			verdict(b), len(b.Statements))
	}
	if len(b.NotDetermined) == 0 {
		t.Fatal("text mode must say what it could not determine")
	}
}

func TestEmptyInputHasNoStatements(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("-- only a comment\n")
	if len(a.Statements) != 0 || len(a.Findings) != 0 ||
		a.ParseMode != changereview.ParseAST && astAvailable() {
		t.Fatalf("comment-only input: %+v", a)
	}
	in := input("")
	in.SQLFiles = nil
	b := e.analyzeWith(e.eng, in)
	if len(b.Statements) != 0 || b.Expectations.PostConditions == nil {
		t.Fatalf("no SQL: %+v (post-conditions must be an empty list, not null)", b)
	}
}
