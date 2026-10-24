package reasoner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
)

// The model stage on a fake provider: citation and grounding of model
// findings (CHECK-RSN-01), the extraction gate (CHECK-RSN-02) and the
// raise-only merge (CHECK-RSN-03). The impact engine is scripted here;
// live_*_test.go runs the same paths on the real engine and store.

// rendered merges res into a and renders the comment the service would.
func rendered(a changereview.Analysis, res changereview.Reasoning) (changereview.Merged,
	string) {
	m := changereview.Merge(a, &res)
	r := changereview.Review{ID: "00000000-0000-0000-0000-0000000000a1", Database: "orders",
		State: changereview.StateComplete, Verdict: m.Verdict,
		DeterministicVerdict: m.DeterministicVerdict, ParseMode: a.ParseMode,
		ModelStatus: m.ModelStatus, Tokens: m.Tokens, Statements: m.Statements,
		Findings: m.Findings, Evidence: m.Evidence, Dropped: m.Dropped,
		NotDetermined: m.NotDetermined, LandingState: changereview.LandingAwaiting,
		ReceiptVerdict: changereview.ReceiptPending, CreditState: changereview.CreditNone}
	return m, changereview.RenderMarkdown(r, changereview.RenderOptions{})
}

func findDropped(res changereview.Reasoning, kind, text string) (changereview.Dropped, bool) {
	for _, d := range res.Dropped {
		if d.Kind == kind && strings.Contains(d.Text, text) {
			return d, true
		}
	}
	return changereview.Dropped{}, false
}

func modelFindings(fs []changereview.Finding) []changereview.Finding {
	var out []changereview.Finding
	for _, f := range fs {
		if f.Origin == changereview.OriginModel {
			out = append(out, f)
		}
	}
	return out
}

// railsRenameRun is CHECK-RSN-01's model run: it translates the Rails
// rename, asks which statements read the column, and submits one cited,
// one uncited and one ungrounded finding.
func railsRenameRun(t *testing.T) []fakeReply {
	return []fakeReply{
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": renameSQL,
			"origin": "translated"})}),
		calls(toolCall{ToolQueriesTouching, `{"relation":"orders","column":"email"}`}),
		submit(func(body string) map[string]any {
			touching := aliasOfKind(t, body, "tool:queries_touching.")
			return map[string]any{
				"statements": []any{map[string]any{
					"evidence_id": aliasFor(t, body, inspectID(changereview.OriginModelTranslated,
						renameSQL)), "origin": "translated", "file": railsMigration, "line": 3,
					"orm": "activerecord", "confidence": 0.9}},
				"findings": []any{
					map[string]any{"severity": "warning", "kind": "breaks_readers",
						"title":        "billing-api still reads email",
						"text":         "queryid 42 ran 412000 calls reading email.",
						"evidence_ids": []string{touching},
						"suggestion": map[string]any{"language": "ruby",
							"code": "add_column :orders, :email_address, :string"}},
					map[string]any{"severity": "warning", "kind": "breaks_readers",
						"title": "Uncited worry", "text": "Something might break.",
						"evidence_ids": []string{}},
					map[string]any{"severity": "warning", "kind": "breaks_readers",
						"title": "Invented number", "text": "queryid 42 ran 999999 calls.",
						"evidence_ids": []string{touching}},
				},
				"intent": "contract",
			}
		}),
	}
}

func TestReason_CitedFindingKeptUncitedAndUngroundedDropped(t *testing.T) {
	fake := newFakeLLM(t, railsRenameRun(t)...)
	tools := newFakeTools()
	r := newTestReasoner(t, Deps{Model: fake.client(), Tools: tools})
	res := reason(t, r, testInput(railsRenameDiff, "rails"), noSQLAnalysis())
	if res.ModelStatus != changereview.ModelOK || res.Transcript == nil ||
		res.Transcript.Stop != agentloop.StopFinal || res.Tokens <= 0 {
		t.Fatalf("status %q, transcript %+v, tokens %d", res.ModelStatus, res.Transcript,
			res.Tokens)
	}
	kept := modelFindings(res.Findings)
	if len(kept) != 1 {
		t.Fatalf("model findings = %+v, want exactly the cited one", kept)
	}
	f := kept[0]
	if f.Title != "billing-api still reads email" || f.Severity != changereview.SeverityWarning ||
		f.RuleID != "" || len(f.EvidenceIDs) != 1 ||
		!strings.HasPrefix(f.EvidenceIDs[0], "tool:queries_touching.") ||
		f.Suggestion == nil || f.Suggestion.Language != "ruby" ||
		f.Suggestion.Code != "add_column :orders, :email_address, :string" {
		t.Fatalf("kept finding = %+v", f)
	}
	if d, ok := findDropped(res, "finding", "Uncited worry"); !ok || d.Reason != "uncited" {
		t.Errorf("uncited finding: %+v (%v), all %+v", d, ok, res.Dropped)
	}
	if d, ok := findDropped(res, "finding", "Invented number"); !ok || d.Reason != "ungrounded" {
		t.Errorf("ungrounded finding: %+v (%v), all %+v", d, ok, res.Dropped)
	}
	if !holds(res.Evidence, f.EvidenceIDs[0]) {
		t.Fatalf("the cited tool result is not in the reasoning's evidence: %v", ids(res.Evidence))
	}
	if len(tools.inspections()) != 1 || tools.origins[0] != changereview.OriginModelTranslated {
		t.Fatalf("inspections %v origins %v", tools.inspections(), tools.origins)
	}
	m, md := rendered(noSQLAnalysis(), res)
	if !strings.Contains(md, "2 model claims or findings were dropped") {
		t.Fatalf("the comment does not show the dropped count:\n%s", md)
	}
	stored := modelFindings(m.Findings)
	if len(stored) != 1 || stored[0].Origin != changereview.OriginModel ||
		stored[0].Severity != changereview.SeverityWarning {
		t.Fatalf("merged model findings = %+v", stored)
	}
	if m.Verdict != changereview.VerdictReview {
		t.Fatalf("verdict = %s, want review", m.Verdict)
	}
}

func TestReason_TranslatedStatementKeepsItsInspection(t *testing.T) {
	fake := newFakeLLM(t, railsRenameRun(t)...)
	r := newTestReasoner(t, Deps{Model: fake.client()})
	res := reason(t, r, testInput(railsRenameDiff, "rails"), noSQLAnalysis())
	if len(res.Statements) != 1 {
		t.Fatalf("statements = %+v, want the translated rename", res.Statements)
	}
	s := res.Statements[0]
	if s.Origin != changereview.OriginModelTranslated || s.File != railsMigration ||
		s.Line != 3 || s.SQL != renameSQL || s.ORM != "activerecord" || s.Confidence != 0.9 ||
		s.EvidenceID != inspectID(changereview.OriginModelTranslated, renameSQL) ||
		s.Index != 1 || s.Fingerprint == "" {
		t.Fatalf("translated statement = %+v", s)
	}
	if !holds(res.Evidence, s.EvidenceID) {
		t.Fatalf("the statement's inspection is not in the evidence: %v", ids(res.Evidence))
	}
	m, _ := rendered(noSQLAnalysis(), res)
	if !hasRule(m.Findings, changereview.RuleModelTranslated) {
		t.Fatalf("translated DDL raised no CR-19: %+v", m.Findings)
	}
	if hasRule(res.Findings, changereview.RuleNotAnalyzableSQL) {
		t.Fatalf("a translated ORM migration still got CR-20: %+v", res.Findings)
	}
}

// gateRun inspects every statement of the extraction gate fixture in one
// turn and submits them all.
func gateRun(t *testing.T, stmts []map[string]any, sqls []string) []fakeReply {
	var inspect []toolCall
	for _, sql := range sqls {
		inspect = append(inspect, toolCall{ToolInspect, jsonArgs(map[string]any{"sql": sql,
			"origin": "extracted"})})
	}
	return []fakeReply{calls(inspect...), submit(func(body string) map[string]any {
		out := make([]any, 0, len(stmts))
		for _, s := range stmts {
			c := map[string]any{}
			for k, v := range s {
				c[k] = v
			}
			if sql, ok := c["sql"].(string); ok {
				c["evidence_id"] = aliasFor(t, body, inspectID(
					changereview.OriginModelExtracted, sql))
				delete(c, "sql")
			}
			out = append(out, c)
		}
		return map[string]any{"statements": out, "findings": []any{}}
	})}
}

// The extraction gate fixture: one statement per gate outcome.
const (
	gateBroken  = "SELEC id FROM orders"
	gateNoPlan  = "SELECT id FROM orders o, LATERAL bad(o) x"
	gateByID    = "SELECT email FROM orders WHERE id = $1"
	gateOther   = "SELECT 1 FROM orders WHERE note IS NULL"
	gateFarAway = "SELECT 2 FROM orders WHERE note IS NULL"
)

func gateFixture(t *testing.T) (*fakeTools, *fakeLLM) {
	tools := newFakeTools()
	tools.parseFail[gateBroken] = "syntax error at or near \"SELEC\""
	tools.planFail[gateNoPlan] = "function bad(orders) does not exist"
	tools.matched[gateByID] = 42
	stmt := func(sql, file string, line int, conf float64) map[string]any {
		return map[string]any{"sql": sql, "origin": "extracted", "file": file, "line": line,
			"orm": "activerecord", "confidence": conf, "in_loop": sql == gateByID}
	}
	stmts := []map[string]any{
		stmt(gateBroken, controllerPath, 12, 0.5),
		stmt(gateNoPlan, controllerPath, 12, 0.5),
		stmt(gateOther, "app/models/other.rb", 12, 0.5),
		stmt(gateFarAway, controllerPath, 17, 0.5),
		stmt(gateByID, controllerPath, 16, -0.2),
		stmt(extractedSQL, controllerPath, 12, 1.7),
		{"evidence_id": "E99", "origin": "extracted", "file": controllerPath, "line": 12},
	}
	return tools, newFakeLLM(t, gateRun(t, stmts, []string{gateBroken, gateNoPlan, gateOther,
		gateFarAway, gateByID, extractedSQL})...)
}

func TestReason_ExtractionGate(t *testing.T) {
	const (
		broken, noPlan, byID = gateBroken, gateNoPlan, gateByID
		other, farAway       = gateOther, gateFarAway
	)
	tools, fake := gateFixture(t)
	r := newTestReasoner(t, Deps{Model: fake.client(), Tools: tools})
	res := reason(t, r, testInput(controllerDiff, "rails"), safeAnalysis())
	if len(res.Statements) != 2 {
		t.Fatalf("accepted %+v, want the queryid match and the new shape", res.Statements)
	}
	matched, fresh := res.Statements[0], res.Statements[1]
	if matched.SQL != byID || matched.MatchedQueryID == nil || *matched.MatchedQueryID != 42 ||
		matched.EvidenceID != inspectID(changereview.OriginModelExtracted, byID) ||
		matched.Fingerprint != "fp"+inspectRef(changereview.OriginModelExtracted, byID) ||
		matched.Index != 2 || matched.Line != 16 || matched.Confidence != 0 ||
		!matched.InLoop || matched.Origin != changereview.OriginModelExtracted {
		t.Fatalf("matched statement = %+v", matched)
	}
	if fresh.SQL != extractedSQL || fresh.MatchedQueryID != nil || fresh.Index != 3 ||
		fresh.Confidence != 1 || fresh.PlanSummary == "" {
		t.Fatalf("new-shape statement = %+v", fresh)
	}
	wantReasons := map[string]string{broken: "not_parsed", noPlan: "not_planned",
		other: "file_not_in_diff", farAway: "line_not_added", "E99": "not_inspected"}
	for text, reason := range wantReasons {
		if d, ok := findDropped(res, "statement", text); !ok || d.Reason != reason {
			t.Errorf("statement %q: dropped %+v (%v), want reason %s", text, d, ok, reason)
		}
	}
	nd := strings.ToLower(notDeterminedText(res))
	for _, want := range []string{"parse", "plan", "not in the diff", "added line",
		"inspect_statement"} {
		if !strings.Contains(nd, want) {
			t.Errorf("not_determined lacks %q: %s", want, nd)
		}
	}
	_, md := rendered(safeAnalysis(), res)
	if !strings.Contains(md, "new shape") || !strings.Contains(md, "matches queryid 42") {
		t.Fatalf("the comment does not label the statements:\n%s", md)
	}
}

func notDeterminedText(res changereview.Reasoning) string {
	var b strings.Builder
	for _, n := range res.NotDetermined {
		b.WriteString(n.Subject + ": " + n.Reason + "\n")
	}
	return b.String()
}

// limitsRun inspects 11 statements in two turns and submits them all with
// 9 findings and 8 not_determined items.
func limitsRun(t *testing.T, sqls []string) []fakeReply {
	var findings []any
	for i := 0; i < MaxFindings+1; i++ {
		findings = append(findings, map[string]any{"severity": "info", "kind": "note",
			"title": "Note " + string(rune('A'+i)), "text": "See the statement.",
			"evidence_ids": []string{"E1"}})
	}
	return []fakeReply{
		calls(inspectCalls(sqls[:6])...), calls(inspectCalls(sqls[6:])...),
		submit(func(body string) map[string]any {
			out := []any{}
			for _, sql := range sqls {
				out = append(out, map[string]any{"evidence_id": aliasFor(t, body, inspectID(
					changereview.OriginModelExtracted, sql)),
					"origin": "extracted", "file": controllerPath, "line": 12})
			}
			return map[string]any{"statements": out, "findings": findings,
				"not_determined": []string{"row 7 unclear", "a", "b", "c", "d", "e", "f", "g"}}
		}),
	}
}

func TestReason_StatementAndFindingLimits(t *testing.T) {
	var sqls []string
	for i := 0; i < MaxStatements+1; i++ {
		sqls = append(sqls, "SELECT "+string(rune('a'+i))+" FROM orders WHERE status = $1")
	}
	b := testBounds()
	b.MaxCost, b.MaxCalls = 20, 20
	r := newTestReasoner(t, Deps{Model: newFakeLLM(t, limitsRun(t, sqls)...).client(),
		Tools: newFakeTools(), Bounds: b})
	res := reason(t, r, testInput(controllerDiff, "rails"), safeAnalysis())
	if len(res.Statements) != MaxStatements {
		t.Fatalf("accepted %d statements, want %d", len(res.Statements), MaxStatements)
	}
	if d, ok := findDropped(res, "statement", sqls[MaxStatements]); !ok ||
		d.Reason != "over_limit" {
		t.Errorf("11th statement: %+v (%v)", d, ok)
	}
	if n := len(modelFindings(res.Findings)); n != MaxFindings {
		t.Fatalf("kept %d findings, want %d", n, MaxFindings)
	}
	if d, ok := findDropped(res, "finding", "Note I"); !ok || d.Reason != "over_limit" {
		t.Errorf("9th finding: %+v (%v)", d, ok)
	}
	var model []string
	for _, n := range res.NotDetermined {
		if n.Reason == ModelNotDeterminedReason {
			model = append(model, n.Subject)
		}
	}
	if strings.Join(model, ",") != "a,b,c,d,e,f" {
		t.Fatalf("model not_determined = %v, want the first 6 without digits", model)
	}
}

func inspectCalls(sqls []string) []toolCall {
	out := make([]toolCall, 0, len(sqls))
	for _, sql := range sqls {
		out = append(out, toolCall{ToolInspect, jsonArgs(map[string]any{"sql": sql,
			"origin": "extracted"})})
	}
	return out
}

func TestReason_SeverityClampAndSuggestionLimit(t *testing.T) {
	long := strings.Repeat("x", MaxSuggestionBytes+1)
	fake := newFakeLLM(t, submit(func(body string) map[string]any {
		stmt := aliasFor(t, body, "stmt:1")
		return map[string]any{"findings": []any{
			map[string]any{"severity": "critical", "kind": "Lock Risk!",
				"title": "The new column needs a look", "text": "Check the statement.",
				"evidence_ids": []string{stmt},
				"suggestion":   map[string]any{"language": "sql", "code": long}},
			map[string]any{"severity": "info", "kind": "note", "title": "Context only",
				"text": "Nothing to do.", "evidence_ids": []string{stmt},
				"suggestion": map[string]any{"language": "sql",
					"code": strings.Repeat("y", MaxSuggestionBytes)}},
		}}
	}))
	r := newTestReasoner(t, Deps{Model: fake.client()})
	res := reason(t, r, testInput("", ""), safeAnalysis())
	kept := modelFindings(res.Findings)
	if len(kept) != 2 {
		t.Fatalf("findings = %+v", kept)
	}
	if kept[0].Severity != changereview.SeverityWarning || kept[0].Kind != "model_finding" ||
		kept[0].Suggestion != nil || kept[0].EvidenceIDs[0] != "stmt:1" {
		t.Fatalf("clamped finding = %+v", kept[0])
	}
	if kept[1].Severity != changereview.SeverityInfo || kept[1].Suggestion == nil ||
		len(kept[1].Suggestion.Code) != MaxSuggestionBytes {
		t.Fatalf("info finding = %+v", kept[1])
	}
	if !strings.Contains(notDeterminedText(res), "suggestion") {
		t.Fatalf("the omitted suggestion is not reported: %+v", res.NotDetermined)
	}
}

func TestReason_IntentAndClaims(t *testing.T) {
	fake := newFakeLLM(t, submit(func(body string) map[string]any {
		stmt := aliasFor(t, body, "stmt:1")
		return map[string]any{"findings": []any{}, "intent": "expand",
			"claims": []any{
				map[string]any{"text": "Statement 1 only adds a nullable column.",
					"evidence_ids": []string{stmt}},
				map[string]any{"text": "It adds 7 indexes.", "evidence_ids": []string{stmt}},
				map[string]any{"text": "No evidence for this.", "evidence_ids": []string{}},
			}}
	}))
	r := newTestReasoner(t, Deps{Model: fake.client()})
	res := reason(t, r, testInput("", ""), safeAnalysis())
	if !strings.HasPrefix(res.Intent, "expand") ||
		!strings.Contains(res.Intent, "Statement 1 only adds a nullable column.") ||
		!strings.Contains(res.Intent, "stmt:1") || strings.Contains(res.Intent, "7 indexes") {
		t.Fatalf("intent = %q", res.Intent)
	}
	if d, ok := findDropped(res, "claim", "7 indexes"); !ok || d.Reason != "ungrounded" {
		t.Errorf("ungrounded claim: %+v (%v)", d, ok)
	}
	if d, ok := findDropped(res, "claim", "No evidence"); !ok || d.Reason != "uncited" {
		t.Errorf("uncited claim: %+v (%v)", d, ok)
	}
	if len(modelFindings(res.Findings)) != 0 {
		t.Fatalf("claims became findings: %+v", res.Findings)
	}
	unknown := newFakeLLM(t, submit(func(string) map[string]any {
		return map[string]any{"findings": []any{}, "intent": "yolo"}
	}))
	res = reason(t, newTestReasoner(t, Deps{Model: unknown.client()}), testInput("", ""),
		safeAnalysis())
	if res.Intent != IntentUnclear {
		t.Fatalf("unknown intent = %q, want %q", res.Intent, IntentUnclear)
	}
}

// Downgrade-only (CHECK-RSN-03): the model can raise safe to review, never
// lower a verdict, never reach risky and never touch a deterministic
// finding.
type raiseCase struct {
	name     string
	analysis changereview.Analysis
	script   []fakeReply
	diff     string
	verdict  string
	rule     string
}

func raiseCases(t *testing.T) []raiseCase {
	warning := func(severity string) fakeReply {
		return submit(func(body string) map[string]any {
			return map[string]any{"findings": []any{map[string]any{"severity": severity,
				"kind": "review_note", "title": "Check the new column",
				"text":         "The new column has no default.",
				"evidence_ids": []string{aliasFor(t, body, "stmt:1")}}}}
		})
	}
	translated := []fakeReply{
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": renameSQL,
			"origin": "translated"})}),
		submit(func(body string) map[string]any {
			return map[string]any{"findings": []any{}, "statements": []any{map[string]any{
				"evidence_id": aliasFor(t, body, inspectID(changereview.OriginModelTranslated,
					renameSQL)), "origin": "translated", "file": railsMigration, "line": 3,
				"confidence": 0.8}}}
		}),
	}
	return []raiseCase{
		{"risky stays risky", riskyAnalysis(), []fakeReply{emptySubmit()}, "",
			changereview.VerdictRisky, ""},
		{"warning raises safe", safeAnalysis(), []fakeReply{warning("warning")}, "",
			changereview.VerdictReview, ""},
		{"critical is a warning", safeAnalysis(), []fakeReply{warning("critical")}, "",
			changereview.VerdictReview, ""},
		{"translated DDL", safeAnalysis(), translated, railsRenameDiff,
			changereview.VerdictReview, changereview.RuleModelTranslated},
	}
}

func TestReason_ModelOnlyRaisesTheVerdict(t *testing.T) {
	for _, c := range raiseCases(t) {
		t.Run(c.name, func(t *testing.T) {
			r := newTestReasoner(t, Deps{Model: newFakeLLM(t, c.script...).client()})
			res := reason(t, r, testInput(c.diff, "rails"), c.analysis)
			m, _ := rendered(c.analysis, res)
			if m.Verdict != c.verdict {
				t.Fatalf("verdict %s, want %s (findings %+v)", m.Verdict, c.verdict, m.Findings)
			}
			checkRaiseOnly(t, c.analysis, m)
			if c.rule != "" && !hasRule(m.Findings, c.rule) {
				t.Fatalf("no %s in %+v", c.rule, m.Findings)
			}
		})
	}
}

// checkRaiseOnly: the deterministic findings lead the merged review
// byte-for-byte, and nothing the model stage added is critical.
func checkRaiseOnly(t *testing.T, a changereview.Analysis, m changereview.Merged) {
	t.Helper()
	// An empty slice, not nil: m.Findings[:0] marshals as [].
	before, err := json.Marshal(append([]changereview.Finding{}, a.Findings...))
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(m.Findings[:len(a.Findings)])
	if err != nil || string(after) != string(before) {
		t.Fatalf("deterministic findings changed:\n%s\n%s", before, after)
	}
	for _, f := range m.Findings[len(a.Findings):] {
		if f.Severity == changereview.SeverityCritical {
			t.Fatalf("a model-stage finding is critical: %+v", f)
		}
	}
}

func hasRule(fs []changereview.Finding, rule string) bool {
	for _, f := range fs {
		if f.RuleID == rule {
			return true
		}
	}
	return false
}

func holds(ev []changereview.Evidence, id string) bool {
	for _, e := range ev {
		if e.ID == id {
			return e.Verify() && changereview.ValidEvidenceID(e.ID)
		}
	}
	return false
}

func ids(ev []changereview.Evidence) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.ID)
	}
	return out
}
