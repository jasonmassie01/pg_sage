package reasoner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The five closed, read-only tools: arguments the reasoner refuses before
// the engine runs, engine failures shown to the model as tool errors, and
// what each result makes citable.

func TestTools_WhatIfIndexMeasuresInspectedStatements(t *testing.T) {
	pct := 37.5
	tools := newFakeTools()
	tools.whatIfPct = &pct
	ddl := "CREATE INDEX CONCURRENTLY ON orders (status, created_at)"
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": extractedSQL,
			"origin": "extracted"})}),
		callTools(func(body string) []toolCall {
			id := inspectID(changereview.OriginModelExtracted, extractedSQL)
			return []toolCall{
				{ToolWhatIf, jsonArgs(map[string]any{"ddl": ddl,
					"statement_evidence_ids": []string{aliasFor(t, body, id)}})},
				{ToolWhatIf, jsonArgs(map[string]any{"ddl": ddl + " WHERE true",
					"statement_evidence_ids": []string{id}})},
				{ToolWhatIf, jsonArgs(map[string]any{"ddl": ddl,
					"statement_evidence_ids": []string{"E77"}})},
				{ToolWhatIf, jsonArgs(map[string]any{"ddl": "DROP INDEX x",
					"statement_evidence_ids": []string{id}})},
			}
		}),
		submit(func(body string) map[string]any {
			return map[string]any{"findings": []any{map[string]any{"severity": "info",
				"kind": "index_benefit", "title": "The index helps the new query",
				"text":         "HypoPG plans it at 37.5% lower cost.",
				"evidence_ids": []string{aliasOfKind(t, body, "whatif:tool.")}}}}
		}))
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	if len(tools.whatIfs) != 3 || len(tools.whatIfs[0]) != 1 ||
		tools.whatIfs[0][0] != extractedSQL || tools.whatIfs[1][0] != extractedSQL {
		t.Fatalf("what-ifs ran with %v, want the inspected SQL twice and the DROP refused "+
			"by the engine", tools.whatIfs)
	}
	if res.Transcript.Rejected["invalid_args"] != 2 {
		t.Fatalf("rejected %+v, want the unknown evidence and the DROP refused",
			res.Transcript.Rejected)
	}
	kept := modelFindings(res.Findings)
	if len(kept) != 1 || !strings.HasPrefix(kept[0].EvidenceIDs[0], "whatif:tool.") {
		t.Fatalf("findings = %+v", kept)
	}
}

func TestTools_WhatIfIndexRefusesTooManyStatements(t *testing.T) {
	var ids []string
	for i := 0; i < 11; i++ {
		ids = append(ids, fmt.Sprintf("E%d", i+1))
	}
	tools := newFakeTools()
	fake := newFakeLLM(t, calls(toolCall{ToolWhatIf, jsonArgs(map[string]any{
		"ddl": "CREATE INDEX ON orders (status)", "statement_evidence_ids": ids})}),
		emptySubmit())
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	if len(tools.whatIfs) != 0 || res.Transcript.Rejected["invalid_args"] != 1 {
		t.Fatalf("what-ifs %v rejected %+v", tools.whatIfs, res.Transcript.Rejected)
	}
}

func TestTools_TableContextChargesUntouchedRelations(t *testing.T) {
	var calls1 []toolCall
	calls1 = append(calls1, toolCall{ToolTableContext, `{"relation":"public.orders"}`})
	for i := 0; i < MaxUntouchedTables+1; i++ {
		calls1 = append(calls1, toolCall{ToolTableContext,
			fmt.Sprintf(`{"relation":"other_%d"}`, i)})
	}
	tools := newFakeTools()
	fake := newFakeLLM(t, calls(calls1...), emptySubmit())
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	if len(tools.contexts) != MaxUntouchedTables+1 || tools.contexts[0] != "public.orders" {
		t.Fatalf("table contexts read %v", tools.contexts)
	}
	if res.Transcript.Rejected["invalid_args"] != 1 {
		t.Fatalf("rejected %+v, want the untouched relation over the limit refused",
			res.Transcript.Rejected)
	}
}

func TestTools_EngineFailuresAreToolErrors(t *testing.T) {
	tools := newFakeTools()
	tools.err = errors.New("catalog read timed out")
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, `{"sql":"SELECT 1","origin":"extracted"}`},
			toolCall{ToolQueriesTouching, `{"relation":"orders"}`},
			toolCall{ToolTableContext, `{"relation":"orders"}`}),
		emptySubmit())
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	errs := 0
	for _, s := range res.Transcript.Steps {
		if s.Status == "tool_error" {
			errs++
		}
	}
	if errs != 3 || res.ModelStatus != changereview.ModelOK {
		t.Fatalf("%d tool errors, status %q: %+v", errs, res.ModelStatus, res.Transcript.Steps)
	}
	if body := fake.all()[1]; !strings.Contains(body, "catalog read timed out") {
		t.Fatal("the model was not shown the engine failure")
	}
}

func TestTools_RefuseBadArguments(t *testing.T) {
	tools := newFakeTools()
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, `{"sql":"SELECT 1","origin":"invented"}`},
			toolCall{ToolInspect, `{"sql":"","origin":"extracted"}`},
			toolCall{ToolInspect, `{"sql":"SELECT 1; SELECT 2;","origin":"extracted"}`},
			toolCall{ToolQueriesTouching, `{"relation":""}`},
			toolCall{ToolQueriesTouching, `{"relation":"missing"}`},
			toolCall{ToolDiffExcerpt, `{"path":"","from_line":1,"to_line":2}`},
			toolCall{ToolTableContext, `{"relation":42}`}),
		emptySubmit())
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Tools: tools}),
		testInput(controllerDiff, ""), safeAnalysis())
	if n := res.Transcript.Rejected["invalid_args"]; n != 6 {
		t.Fatalf("rejected %+v, want 6 invalid_args", res.Transcript.Rejected)
	}
	if strings.Join(tools.inspections(), "|") != "SELECT 1; SELECT 2;" {
		t.Fatalf("inspected %q, want only the two-statement SQL the engine refuses",
			tools.inspections())
	}
	if len(tools.touching) != 1 || tools.touching[0] != "missing." {
		t.Fatalf("queries_touching ran for %v", tools.touching)
	}
}

func TestTools_QueriesTouchingIsCitable(t *testing.T) {
	fake := newFakeLLM(t, calls(toolCall{ToolQueriesTouching,
		`{"relation":"orders","column":"email"}`}),
		submit(func(body string) map[string]any {
			return map[string]any{"findings": []any{map[string]any{"severity": "warning",
				"kind": "readers", "title": "billing-api reads email",
				"text":         "queryid 42 peaks at 31.5 calls per second.",
				"evidence_ids": []string{aliasOfKind(t, body, "tool:queries_touching.")}}}}
		}))
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}),
		testInput(controllerDiff, ""), safeAnalysis())
	kept := modelFindings(res.Findings)
	if len(kept) != 1 {
		t.Fatalf("findings %+v dropped %+v", res.Findings, res.Dropped)
	}
	var data string
	for _, e := range res.Evidence {
		if e.ID == kept[0].EvidenceIDs[0] {
			data = string(e.Data)
		}
	}
	for _, want := range []string{`"queryid:42"`, `"calls":412000`, `"relation":"orders"`,
		`"column":"email"`} {
		if !strings.Contains(data, want) {
			t.Errorf("queries_touching evidence lacks %s: %s", want, data)
		}
	}
}
