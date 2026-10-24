package reasoner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// Post-test audit additions: the seed's order and cap, the alias mirror
// the tools resolve aliases by, duplicates, and that a statement's origin
// is its inspection's, whatever the final answer says.

func TestReason_SeedPrefersCitedEvidenceAndIsCapped(t *testing.T) {
	a := noSQLAnalysis()
	for i := 0; i < 40; i++ {
		a.Evidence = append(a.Evidence, mustEvidence("table", fmt.Sprintf("public.t%d", i),
			"table", map[string]any{"i": i}))
	}
	a.Findings = []changereview.Finding{{RuleID: "CR-13", Kind: "published_table",
		Severity: changereview.SeverityInfo, Origin: changereview.OriginRule,
		Title: "published", EvidenceIDs: []string{"table:public.t39"}}}
	fake := newFakeLLM(t, emptySubmit())
	reason(t, newTestReasoner(t, Deps{Model: fake.client()}), testInput("", ""), a)
	body := fake.all()[0]
	if got := aliasFor(t, body, "table:public.t39"); got != "E1" {
		t.Fatalf("the cited evidence is %s, want E1", got)
	}
	p := prompt(t, body)
	if !strings.Contains(p, "[table:public.t28 ") || strings.Contains(p, "[table:public.t29 ") {
		t.Fatalf("the seed is not the cited fact plus the first 29 others:\n%s", p)
	}
	if !strings.Contains(p, "F1 CR-13 info: published") || !strings.Contains(p, "(evidence E1)") {
		t.Fatalf("the findings list does not cite the seed alias:\n%s", p)
	}
}

func TestReason_AliasMirrorSurvivesRefusedAndFailedCalls(t *testing.T) {
	tools := newFakeTools()
	pct := 12.5
	tools.whatIfPct = &pct
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, `{"sql":"SELECT 1","origin":"nope"}`},
			toolCall{ToolQueriesTouching, `{"relation":"missing"}`},
			toolCall{ToolTableContext, `{"relation":"orders"}`},
			toolCall{ToolInspect, jsonArgs(map[string]any{"sql": extractedSQL,
				"origin": "extracted"})}),
		callTools(func(body string) []toolCall {
			alias := aliasFor(t, body, inspectID(changereview.OriginModelExtracted,
				extractedSQL))
			return []toolCall{{ToolWhatIf, jsonArgs(map[string]any{
				"ddl": "CREATE INDEX ON orders (status)", "statement_evidence_ids": []string{
					alias}})}}
		}),
		emptySubmit())
	reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	if len(tools.whatIfs) != 1 || len(tools.whatIfs[0]) != 1 ||
		tools.whatIfs[0][0] != extractedSQL {
		t.Fatalf("the alias resolved to %v, want the inspected statement", tools.whatIfs)
	}
}

func TestReason_DuplicatesAreDropped(t *testing.T) {
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": extractedSQL,
			"origin": "extracted"})}),
		submit(func(body string) map[string]any {
			id := aliasFor(t, body, inspectID(changereview.OriginModelExtracted, extractedSQL))
			st := map[string]any{"evidence_id": id, "origin": "extracted",
				"file": controllerPath, "line": 12}
			f := map[string]any{"severity": "info", "kind": "note", "title": "Same title",
				"text": "Once.", "evidence_ids": []string{id}}
			return map[string]any{"statements": []any{st, st}, "findings": []any{f, f}}
		}))
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}),
		testInput(controllerDiff, ""), safeAnalysis())
	if len(res.Statements) != 1 || len(modelFindings(res.Findings)) != 1 {
		t.Fatalf("statements %+v findings %+v", res.Statements, res.Findings)
	}
	if d, ok := findDropped(res, "statement", extractedSQL); !ok || d.Reason != "duplicate" {
		t.Errorf("duplicate statement: %+v (%v)", d, ok)
	}
	if d, ok := findDropped(res, "finding", "Same title"); !ok || d.Reason != "duplicate" {
		t.Errorf("duplicate finding: %+v (%v)", d, ok)
	}
}

func TestReason_StatementOriginIsTheInspections(t *testing.T) {
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": extractedSQL,
			"origin": "extracted"})}),
		submit(func(body string) map[string]any {
			return map[string]any{"findings": []any{}, "statements": []any{map[string]any{
				"evidence_id": aliasFor(t, body, inspectID(changereview.OriginModelExtracted,
					extractedSQL)), "origin": "translated", "file": controllerPath,
				"line": 12, "orm": "Active Record!"}}}
		}))
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}),
		testInput(controllerDiff, ""), safeAnalysis())
	if len(res.Statements) != 1 || res.Statements[0].Origin != changereview.OriginModelExtracted ||
		res.Statements[0].ORM != "" {
		t.Fatalf("statements %+v", res.Statements)
	}
	m, _ := rendered(safeAnalysis(), res)
	if hasRule(m.Findings, changereview.RuleModelTranslated) {
		t.Fatal("an extracted statement relabelled translated raised CR-19")
	}
}

func TestReason_RuleHitsOnAcceptedStatementsAreWarnings(t *testing.T) {
	tools := newFakeTools()
	hit := mustEvidence("lockq", "1.1", "lock queue", map[string]any{"queued_estimate": 9000})
	tools.hits[renameSQL] = []changereview.Finding{{RuleID: "CR-02",
		Kind: "lock_queue_exposure", Severity: changereview.SeverityCritical,
		Origin: changereview.OriginRule, Title: "9000 statements queue",
		EvidenceIDs: []string{hit.ID}}}
	tools.hitEv[renameSQL] = []changereview.Evidence{hit}
	fake := newFakeLLM(t, railsRenameRun(t)...)
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(railsRenameDiff, "rails"), noSQLAnalysis())
	var f *changereview.Finding
	for i := range res.Findings {
		if strings.HasPrefix(res.Findings[i].Title, "CR-02 on model-translated SQL") {
			f = &res.Findings[i]
		}
	}
	if f == nil || f.Severity != changereview.SeverityWarning ||
		f.Origin != changereview.OriginModel || !holds(res.Evidence, hit.ID) {
		t.Fatalf("rule hit = %+v, evidence %v", f, ids(res.Evidence))
	}
}

func TestReason_PRTextIsRedacted(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	in := testInput(controllerDiff, "")
	in.PR.Title = "Rotate DB_PASSWORD=hunter2xyz"
	in.PR.Repo = "postgres://ci:s3cretpw@db/acme"
	reason(t, newTestReasoner(t, Deps{Model: fake.client()}), in, safeAnalysis())
	body := fake.all()[0]
	for _, leak := range []string{"hunter2xyz", "s3cretpw"} {
		if strings.Contains(body, leak) {
			t.Errorf("the request carries the PR secret %q", leak)
		}
	}
}

func TestTools_WhatIfRefusesAnUnparsedInspection(t *testing.T) {
	tools := newFakeTools()
	tools.parseFail[gateBroken] = "syntax error"
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": gateBroken,
			"origin": "extracted"})}),
		callTools(func(body string) []toolCall {
			return []toolCall{{ToolWhatIf, jsonArgs(map[string]any{
				"ddl": "CREATE INDEX ON orders (status)", "statement_evidence_ids": []string{
					aliasFor(t, body, inspectID(changereview.OriginModelExtracted,
						gateBroken))}})}}
		}),
		emptySubmit())
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	if len(tools.whatIfs) != 0 || res.Transcript.Rejected["invalid_args"] != 1 {
		t.Fatalf("what-ifs %v rejected %+v", tools.whatIfs, res.Transcript.Rejected)
	}
}
