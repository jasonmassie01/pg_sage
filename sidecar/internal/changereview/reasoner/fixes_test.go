package reasoner

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The reasoner fixes: what the model is shown of the deterministic
// evidence and of tool results is redacted and stripped of SQL literals
// like the migration SQL itself, and DDL the model labels "extracted" is
// still model-derived DDL (CR-19).

const (
	subscriptionSQL = "CREATE SUBSCRIPTION s CONNECTION 'host=db password=Pr0dS3cret' " +
		"PUBLICATION p"
	awsSQL = "SELECT aws_commons.create_aws_credentials('AKIAIOSFODNN7EXAMPLE', " +
		"'wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY', '')"
)

var seedLeaks = []string{"Pr0dS3cret", "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMIK7MDENG"}

// secretAnalysis is a deterministic analysis whose statement evidence
// carries the submitted SQL verbatim, secrets included.
func secretAnalysis() changereview.Analysis {
	a := safeAnalysis()
	for i, sql := range []string{subscriptionSQL, awsSQL} {
		ev := mustEvidence("stmt", string(rune('2'+i)), "statement", map[string]any{
			"index": i + 2, "kind": "other", "sql": sql})
		a.Evidence = append(a.Evidence, ev)
		a.Statements = append(a.Statements, changereview.Statement{Index: i + 2,
			Origin: changereview.OriginSQLInput, SQL: sql, Kind: "other", EvidenceID: ev.ID})
	}
	a.Findings = []changereview.Finding{{RuleID: "CR-17", Kind: "impact_unknown",
		Severity: changereview.SeverityWarning, Origin: changereview.OriginRule,
		Title: "Impact unknown", Text: "not modeled", EvidenceIDs: []string{"stmt:2",
			"stmt:3"}}}
	return a
}

func TestReason_SeedEvidenceIsRedacted(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	in := testInput("", "")
	in.SQLFiles = []changereview.SQLFile{{Path: "db/1.sql", Content: subscriptionSQL +
		";\n" + awsSQL}}
	reason(t, newTestReasoner(t, Deps{Model: fake.client()}), in, secretAnalysis())
	bodies := fake.all()
	if len(bodies) == 0 {
		t.Fatal("no request reached the provider")
	}
	for i, body := range bodies {
		for _, leak := range seedLeaks {
			if strings.Contains(body, leak) {
				t.Errorf("request %d carries %q", i, leak)
			}
		}
	}
	if !strings.Contains(prompt(t, bodies[0]), "stmt:2") {
		t.Fatal("the seed evidence is not offered at all; the check proves nothing")
	}
}

// A tool result carrying a secret (a workload statement stored verbatim)
// reaches the model redacted.
func TestReason_ToolResultsAreRedacted(t *testing.T) {
	tools := newFakeTools()
	tools.touchText = "ALTER ROLE app PASSWORD 'Pr0dS3cret'"
	fake := newFakeLLM(t,
		calls(toolCall{ToolQueriesTouching, `{"relation":"orders"}`}),
		emptySubmit())
	reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	bodies := fake.all()
	if len(bodies) != 2 || !strings.Contains(bodies[1], "ALTER ROLE app") {
		t.Fatalf("%d requests; the tool result must reach the second", len(bodies))
	}
	for i, body := range bodies {
		if strings.Contains(body, "Pr0dS3cret") {
			t.Errorf("request %d carries the tool result's password", i)
		}
	}
}

// DDL the model calls "extracted" is treated as translated: it keeps its
// evidence, is stored as model-translated and raises CR-19.
func TestReason_ExtractedDDLIsModelTranslated(t *testing.T) {
	const ddl = "CREATE TABLE rv.audit (id bigint PRIMARY KEY, note text)"
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": ddl,
			"origin": "extracted"})}),
		submit(func(body string) map[string]any {
			return map[string]any{"statements": []any{map[string]any{
				"evidence_id": aliasFor(t, body, inspectID(changereview.OriginModelExtracted,
					ddl)), "origin": "extracted", "file": railsMigration, "line": 3,
				"orm": "activerecord", "confidence": 0.9}}, "findings": []any{},
				"intent": "expand"}
		}))
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}),
		testInput(railsRenameDiff, "rails"), safeAnalysis())
	if len(res.Statements) != 1 ||
		res.Statements[0].Origin != changereview.OriginModelTranslated {
		t.Fatalf("statements %+v, want the DDL kept as model-translated", res.Statements)
	}
	m, _ := rendered(safeAnalysis(), res)
	cr19 := false
	for _, f := range m.Findings {
		cr19 = cr19 || f.RuleID == changereview.RuleModelTranslated
	}
	if !cr19 || m.Verdict != changereview.VerdictReview {
		t.Fatalf("verdict %s, CR-19 %v; model-derived DDL is never safe", m.Verdict, cr19)
	}
}
