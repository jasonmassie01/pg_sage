package changereview

import (
	"strings"
	"testing"
)

// Pure parts of the review fixes: what counts as model-derived DDL, the
// DBA verdict line of the comment and the UI link. Stateless: no
// concurrency tests.

func TestIsModelDDL(t *testing.T) {
	cases := []struct {
		origin, kind string
		want         bool
	}{
		{OriginModelTranslated, "rename_column", true},
		{OriginModelTranslated, "select", true},
		{OriginModelExtracted, "select", false},
		{OriginModelExtracted, "dml_update", false},
		{OriginModelExtracted, "dml_insert", false},
		{OriginModelExtracted, "dml_delete", false},
		{OriginModelExtracted, "merge", false},
		{OriginModelExtracted, "create_table", true},
		{OriginModelExtracted, "truncate", true},
		{OriginModelExtracted, "drop_schema", true},
		{OriginModelExtracted, "alter_table", true},
		{OriginSQLInput, "create_table", false},
		{OriginModelExtracted, "", true},
	}
	for _, c := range cases {
		if got := IsModelDDL(Statement{Origin: c.origin, Kind: c.kind}); got != c.want {
			t.Errorf("IsModelDDL(%s, %q) = %v, want %v", c.origin, c.kind, got, c.want)
		}
	}
}

// DDL the model labels "extracted" is still model-derived DDL: CR-19,
// never safe.
func TestMergeRaisesCR19ForMislabeledModelDDL(t *testing.T) {
	a := Analysis{ParseMode: ParseAST}
	r := &Reasoning{ModelStatus: ModelOK, Statements: []Statement{{Index: 2,
		Origin: OriginModelExtracted, File: "db/migrate/2_audit.rb", Line: 4,
		SQL: "CREATE TABLE rv.audit (id bigint PRIMARY KEY, note text)",
		Kind: "create_table", EvidenceID: "inspect:abc"}}}
	m := Merge(a, r)
	if m.Verdict != VerdictReview {
		t.Fatalf("verdict %s, want review for model-derived DDL", m.Verdict)
	}
	found := false
	for _, f := range m.Findings {
		found = found || f.RuleID == RuleModelTranslated
	}
	if !found {
		t.Fatalf("no CR-19 for DDL the model labeled extracted: %+v", m.Findings)
	}
	read := &Reasoning{ModelStatus: ModelOK, Statements: []Statement{{Index: 2,
		Origin: OriginModelExtracted, Kind: "select", SQL: "SELECT 1 FROM rv.t",
		EvidenceID: "inspect:def"}}}
	if m := Merge(a, read); m.Verdict != VerdictSafe {
		t.Fatalf("an extracted read: verdict %s, want safe", m.Verdict)
	}
}

func TestRenderedDBAVerdictCarriesTheReasonAndDirection(t *testing.T) {
	r := renderBase(t)
	r.Verdict, r.DeterministicVerdict = VerdictRisky, VerdictRisky
	r.DBAVerdict, r.DBAActor = DBAConcurWithEdits, "alice"
	r.DBAReason = "add SET lock_timeout = '2s' and build the index CONCURRENTLY"
	md := RenderMarkdown(r, RenderOptions{})
	if !strings.Contains(md, "DBA verdict: concur with edits by alice") ||
		!strings.Contains(md, "Edits: add SET lock_timeout = '2s' and build the index "+
			"CONCURRENTLY") {
		t.Fatalf("concur with edits renders:\n%s", md)
	}
	r.DBAVerdict, r.DBADirection, r.DBAReason = DBAOverride, DirectionTooLenient,
		"the table is hot at 9am"
	md = RenderMarkdown(r, RenderOptions{})
	if !strings.Contains(md, "DBA verdict: override (pg_sage was too lenient) by alice") ||
		!strings.Contains(md, "Reason: the table is hot at 9am") {
		t.Fatalf("override renders:\n%s", md)
	}
	r.DBAVerdict, r.DBADirection, r.DBAReason = DBAConcur, "", ""
	if md := RenderMarkdown(r, RenderOptions{}); !strings.Contains(md,
		"DBA verdict: concur by alice.") || strings.Contains(md, "Reason:") {
		t.Fatalf("a plain concurrence renders:\n%s", md)
	}
}

// Without a configured UI address there is no link to give: a relative
// "/#/reviews" link resolves against github.com or a chat client, so the
// comment and the notification name the review instead.
func TestReviewLinkNeedsTheUIAddress(t *testing.T) {
	r := Review{ID: renderID, Database: "shop"}
	if got := ReviewLink(r, RenderOptions{}); got != "" {
		t.Fatalf("ReviewLink without a UI address = %q, want none", got)
	}
	want := "https://sage.example.com/#/reviews?id=" + renderID + "&database=shop"
	if got := ReviewLink(r, RenderOptions{UIBaseURL: "https://sage.example.com/"}); got !=
		want {
		t.Fatalf("ReviewLink = %q, want %q", got, want)
	}
	big := renderBase(t)
	big.Verdict, big.DeterministicVerdict = VerdictRisky, VerdictRisky
	for i := 0; i < 400; i++ {
		f := ruleFinding("CR-17", SeverityWarning, "stmt:1")
		f.Title = strings.Repeat("x", 200)
		big.Findings = append(big.Findings, f)
	}
	md := RenderMarkdown(big, RenderOptions{MaxRunes: 3000})
	if strings.Contains(md, "(/#/reviews") || strings.Contains(md, ": /#/reviews") ||
		!strings.Contains(md, "review "+renderID) {
		t.Fatalf("truncation note without a UI address:\n%s", md[len(md)-300:])
	}
}
