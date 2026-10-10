package agentposture

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// stubQuerier satisfies Querier for tests that never reach the database.
type stubQuerier struct{}

func (*stubQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("stub querier: no database")
}

func (*stubQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

func TestFinding_Category(t *testing.T) {
	f := okFinding("AP-03", Critical)
	if got := f.Category(); got != "agent_posture:AP-03" {
		t.Fatalf("category = %q", got)
	}
	if !IsCategory(f.Category()) || IsCategory("schema_lint:AP-03") || IsCategory("") {
		t.Fatal("IsCategory does not recognise exactly the posture categories")
	}
}

func TestAnalyzerFinding_CarriesIdentityAndNeverExecutableSQL(t *testing.T) {
	f := Finding{Detector: "AP-05", Severity: Warning, ObjectType: "function",
		Object: "public.f(integer)", Title: "f runs as its owner",
		Detail: "SECURITY DEFINER without a pinned search_path", Recommendation: "Pin it",
		FixScript: "ALTER FUNCTION public.f(integer) SET search_path = pg_catalog, pg_temp;",
		Caveat:    "test the function after", Evidence: []Evidence{{Source: "pg_proc",
			Ref: "public.f(integer)", Detail: "prosecdef"}}}
	a := f.AnalyzerFinding()
	if a.Category != "agent_posture:AP-05" || a.RuleID != "AP-05" ||
		a.Severity != "warning" || a.ObjectType != "function" ||
		a.ObjectIdentifier != "public.f(integer)" || a.Title != f.Title ||
		a.Recommendation != "Pin it" {
		t.Fatalf("identity not carried: %+v", a)
	}
	// G0-05: the executor acts only on RecommendedSQL; the fix stays a
	// manual script in the detail.
	if a.RecommendedSQL != "" || a.RollbackSQL != "" {
		t.Fatalf("posture finding carries executable SQL: %q / %q", a.RecommendedSQL,
			a.RollbackSQL)
	}
	if a.Detail["manual_script"] != f.FixScript {
		t.Fatalf("manual_script = %v", a.Detail["manual_script"])
	}
	if a.Detail["proposal_level"] != "L1" || a.Detail["section"] != "agent_posture" {
		t.Fatalf("detail = %v, want proposal_level L1 in section agent_posture", a.Detail)
	}
	if a.Detail["detector"] != "AP-05" || a.Detail["caveat"] != f.Caveat ||
		a.Detail["detail"] != f.Detail {
		t.Fatalf("detail = %v", a.Detail)
	}
	ev, ok := a.Detail["evidence"].([]Evidence)
	if !ok || len(ev) != 1 || ev[0].Ref != "public.f(integer)" {
		t.Fatalf("evidence = %#v", a.Detail["evidence"])
	}
}

func TestAnalyzerFinding_EmptyFixAndNilEvidence(t *testing.T) {
	a := Finding{Detector: "AP-08", Severity: Info, ObjectType: "role", Object: "app",
		Title: "no timeouts"}.AnalyzerFinding()
	if _, ok := a.Detail["manual_script"]; ok {
		t.Fatal("an empty fix script must not be recorded")
	}
	if a.RecommendedSQL != "" || a.Detail["proposal_level"] != ProposalLevel {
		t.Fatalf("finding = %+v", a)
	}
}

func TestProposalLevelIsL1(t *testing.T) {
	if ProposalLevel != "L1" {
		t.Fatalf("ProposalLevel = %q: posture findings never propose above L1 (G0-05)",
			ProposalLevel)
	}
}

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"orders":       "orders",
		"Orders":       `"Orders"`,
		"user":         `"user"`,
		"my table":     `"my table"`,
		`we"ird`:       `"we""ird"`,
		"1abc":         `"1abc"`,
		"":             `""`,
		"snake_case_1": "snake_case_1",
	}
	for in, want := range cases {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
	if got := QualifiedName("App", "t"); got != `"App".t` {
		t.Fatalf("QualifiedName = %s", got)
	}
}

func TestStatementCarriesTheFirstLookTag(t *testing.T) {
	s := Statement("AP-03", "SELECT 1")
	if !strings.HasPrefix(s, "/* pg_sage first_look */ ") {
		t.Fatalf("statement %q does not start with the first look tag", s)
	}
	if !strings.Contains(s, "agent_posture AP-03") || !strings.HasSuffix(s, "SELECT 1") {
		t.Fatalf("statement = %q", s)
	}
}
