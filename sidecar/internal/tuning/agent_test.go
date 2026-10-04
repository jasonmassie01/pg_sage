package tuning

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The agent's cycle: classify, detect cases, re-emit what is still open,
// ask the model about the rest within the budget, validate, calibrate,
// rank.

func indexAnswer(t *testing.T) func([]llm.Message) (llm.ToolResult, error) {
	return answer(proposalsJSON(t, map[string]any{"type": "index_create",
		"ddl":       "CREATE INDEX CONCURRENTLY orders_customer_idx ON public.orders (customer_id)",
		"rationale": "seq scan on customer_id", "evidence": []string{"S1", "R1"},
		"expected_change_pct": -50, "target_queryids": []int64{101}}))
}

func tune(t *testing.T, h *harness) analyzer.TuningOutput {
	t.Helper()
	prev, cur := ordersPair()
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	return out
}

func TestTune_NoCasesNeverCallsTheModel(t *testing.T) {
	h := newHarness(t)
	prev, cur := shareSnaps(10, 100, 5) // 100 ms of workload: no case
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	if h.model.callCount() != 0 || len(out.Findings) != 0 {
		t.Fatalf("calls %d findings %v: no case, no model", h.model.callCount(), out.Findings)
	}
	if !slices.Contains(out.Evaluated, optimizer.OptimizerCategory) || len(out.Failed) != 0 {
		t.Fatalf("a quiet cycle is still an evaluation: %+v", out)
	}
}

func TestTune_OneCaseAdmitsAnIndex(t *testing.T) {
	h := newHarness(t, toolCall("statement", `{"queryid":101}`), indexAnswer(t))
	out := tune(t, h)
	if h.model.callCount() != 2 {
		t.Fatalf("model calls = %d, want 2 (a tool turn and the answer)", h.model.callCount())
	}
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok {
		t.Fatalf("findings = %+v", out.Findings)
	}
	d := f.Detail
	if d["producer"] != Producer || d["case_id"] != "top_statement:101" ||
		d["case_kind"] != string(CaseTopStatement) || d["proposal_type"] != "index_create" {
		t.Fatalf("detail = %v", d)
	}
	pe, _ := d["predicted_effect"].(map[string]any)
	if pe["method"] != "hypopg" || pe["source"] != PredictionSource {
		t.Fatalf("predicted_effect = %v", d["predicted_effect"])
	}
	cal, _ := d["confidence_calibration"].(map[string]any)
	if cal["status"] != StatusUncalibrated {
		t.Fatalf("no outcomes yet: %v", d["confidence_calibration"])
	}
	if _, has := d["confidence_score"]; has {
		t.Fatal("an uncalibrated finding carries no confidence number")
	}
	ev, _ := d["evidence"].([]Evidence)
	if len(ev) != 2 || ev[0].ID != "S1" || ev[1].ID != "R1" || ev[1].Kind != "tool:statement" {
		t.Fatalf("cited evidence = %+v", d["evidence"])
	}
	if !slices.Contains(out.IndexTables, "public.orders") {
		t.Fatalf("index tables = %v (the tuner defers them)", out.IndexTables)
	}
}

func TestTune_PromptIsBoundedUntrustedDataWithTools(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	tune(t, h)
	msgs := h.model.msgs[0]
	if !strings.Contains(msgs[0].Content, llm.UntrustedDataRule) {
		t.Fatal("the system prompt carries the untrusted-data rule")
	}
	user := msgs[1].Content
	if !strings.Contains(user, `<data label="tuning_case">`) || !strings.Contains(user, "S1") ||
		!strings.Contains(user, "top_statement:101") {
		t.Fatalf("user packet = %q", user)
	}
	if len(user) > maxPacketBytes+512 {
		t.Fatalf("packet %d bytes over the bound", len(user))
	}
	var names []string
	for _, tool := range h.model.tools[0] {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"statement", "table", "explain", "whatif_index",
		"write_cost", "extended_stats"} {
		if !slices.Contains(names, want) {
			t.Fatalf("tools = %v, missing %s", names, want)
		}
	}
	if slices.Contains(names, "rehearse") {
		t.Fatal("no clone provider: no rehearsal tool")
	}
}

func TestTune_CalibratedConfidenceOnTheFinding(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.store.outcomes = improvedOutcomes("index_create", "hypopg", -40, 6, 6)
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if f.Detail["confidence_score"] != 1.0 {
		t.Fatalf("confidence_score = %v, want the observed 6/6", f.Detail["confidence_score"])
	}
	cal, _ := f.Detail["confidence_calibration"].(map[string]any)
	if cal["status"] != StatusCalibrated || cal["basis"] != BasisBin || cal["n"] != 6 {
		t.Fatalf("calibration = %v", cal)
	}
	if _, gated := f.Detail[analyzer.DetailApprovalRequired]; gated {
		t.Fatal("6/6 clears the threshold")
	}
	if h.store.outCalls != 1 {
		t.Fatalf("outcomes read %d times, want once per cycle", h.store.outCalls)
	}
}

func TestTune_CalibratedBelowThresholdNeedsApproval(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.store.outcomes = improvedOutcomes("index_create", "hypopg", -40, 5, 2)
	out := tune(t, h)
	f, _ := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	why, gated := f.Detail[analyzer.DetailApprovalRequired].(string)
	if !gated || !strings.Contains(why, "2 of 5") {
		t.Fatalf("2/5 improved: an operator decides (%v)", f.Detail)
	}
}

func TestTune_RankingAndCap(t *testing.T) {
	s := defaultSettings()
	s.Tuning.MaxProposalsPerCycle = 2
	h := newHarnessWith(t, s, answer(proposalsJSON(t,
		map[string]any{"type": "reloption", "table": "public.orders",
			"option": "autovacuum_vacuum_scale_factor", "value": "0.02",
			"evidence": []string{"T1"}, "expected_change_pct": -40},
		map[string]any{"type": "guc", "name": "work_mem", "value": "64MB",
			"evidence": []string{"S1"}, "expected_change_pct": -60},
		map[string]any{"type": "index_create",
			"ddl":      "CREATE INDEX CONCURRENTLY orders_c_idx ON public.orders (customer_id)",
			"evidence": []string{"S1"}, "expected_change_pct": -50})))
	h.store.outcomes = append(improvedOutcomes("index_create", "hypopg", -40, 5, 5),
		improvedOutcomes("reloption", "model", -40, 5, 0)...)
	out := tune(t, h)
	if len(out.Findings) != 2 {
		t.Fatalf("findings = %d, want the cap 2", len(out.Findings))
	}
	if out.Findings[0].Category != optimizer.OptimizerCategory ||
		out.Findings[1].Category != "memory_tuning" {
		t.Fatalf("order = %s, %s: calibrated-high, then uncalibrated; calibrated-low "+
			"is cut", out.Findings[0].Category, out.Findings[1].Category)
	}
	if !h.logs.contains("cap") {
		t.Fatal("cutting proposals at the cap is logged")
	}
}

func agentFinding(caseID, cat, ident string) analyzer.Finding {
	return analyzer.Finding{Category: cat, Severity: "info", ObjectType: "index",
		ObjectIdentifier: ident, Title: "open",
		Detail: map[string]any{"producer": Producer, "case_id": caseID,
			"table": "public.orders"},
		RecommendedSQL: "CREATE INDEX CONCURRENTLY x ON public.orders (customer_id)"}
}

func TestTune_OpenFindingsAreReemittedAndSkipTheModel(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	open := agentFinding("top_statement:101", optimizer.OptimizerCategory,
		"public.orders|btree(customer_id)")
	h.store.open = []analyzer.Finding{open}
	out := tune(t, h)
	if h.model.callCount() != 0 {
		t.Fatal("a case with an open proposal is not asked again")
	}
	if len(out.Findings) != 1 || out.Findings[0].ObjectIdentifier != open.ObjectIdentifier ||
		out.Findings[0].RecommendedSQL != open.RecommendedSQL {
		t.Fatalf("findings = %+v: the open proposal is re-emitted unchanged", out.Findings)
	}
	if !slices.Contains(out.IndexTables, "public.orders") {
		t.Fatalf("index tables = %v", out.IndexTables)
	}
}

func TestTune_OpenFindingOfAVanishedCaseDoesNotBlockOtherCases(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.store.open = []analyzer.Finding{agentFinding("top_statement:555",
		optimizer.OptimizerCategory, "public.orders|btree(status)")}
	out := tune(t, h)
	if _, ok := byIdent(out.Findings, "public.orders|btree(status)"); !ok {
		t.Fatal("its case was not examined this cycle: the open finding is kept")
	}
	if h.model.callCount() == 0 {
		t.Fatal("case 101 has no open proposal of its own: the model is asked")
	}
}

func TestTune_LegacyOptimizerFindingsFollowTheirTable(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	onCase := analyzer.Finding{Category: optimizer.OptimizerCategory, ObjectType: "index",
		ObjectIdentifier: "public.orders|btree(status)",
		Detail:           map[string]any{"table": "public.orders", "llm_rationale": "old"}}
	offCase := onCase
	offCase.ObjectIdentifier = "public.other|btree(x)"
	offCase.Detail = map[string]any{"table": "public.other", "llm_rationale": "old"}
	h.store.open = []analyzer.Finding{onCase, offCase}
	out := tune(t, h)
	var idents []string
	for _, f := range out.Findings {
		idents = append(idents, f.ObjectIdentifier)
	}
	if !slices.Contains(idents, onCase.ObjectIdentifier) ||
		!slices.Contains(idents, offCase.ObjectIdentifier) {
		t.Fatalf("re-emitted = %v: pre-agent index findings stay open, and one on a "+
			"case's table keeps that case from being asked", idents)
	}
	if h.model.callCount() != 0 {
		t.Fatal("the table already has an open index proposal: no new model call")
	}
}

// threeCases is an interval with three top statements.
func threeCases() (prev, cur *collector.Snapshot) {
	tbl := []collector.TableStats{table("public", "orders", 1000, 0)}
	var p, c []collector.QueryStats
	for i := int64(1); i <= 3; i++ {
		q := fmt.Sprintf("SELECT * FROM public.orders WHERE c%d = $1", i)
		p = append(p, stmt(i, q, 100, 1000))
		c = append(c, stmt(i, q, 200, 1000+float64(i)*1000))
	}
	return snapAt(t0, p, tbl, nil), snapAt(t0.Add(time.Minute), c, tbl, nil)
}

func TestTune_MaxCasesPerCycle(t *testing.T) {
	s := defaultSettings()
	s.Tuning.MaxCasesPerCycle = 1
	h := newHarnessWith(t, s)
	prev, cur := threeCases()
	if _, err := h.agent.Tune(context.Background(), cur, prev); err != nil {
		t.Fatalf("tune: %v", err)
	}
	if h.model.callCount() != 1 {
		t.Fatalf("model calls = %d, want 1 (one case per cycle)", h.model.callCount())
	}
	if !h.logs.contains("2 case(s) deferred to a later cycle") {
		t.Fatalf("logs = %v", h.logs.lines)
	}
}

func TestTune_RequestBudgetSpansCases(t *testing.T) {
	s := defaultSettings()
	s.Tuning.MaxRequestsPerCycle = 2
	h := newHarnessWith(t, s, toolCall("statement", `{"queryid":3}`),
		answer(`{"proposals":[]}`), answer(`{"proposals":[]}`))
	prev, cur := threeCases()
	if _, err := h.agent.Tune(context.Background(), cur, prev); err != nil {
		t.Fatalf("tune: %v", err)
	}
	if h.model.callCount() != 2 {
		t.Fatalf("model calls = %d, want the cycle's request budget 2", h.model.callCount())
	}
	if !h.logs.contains("budget") {
		t.Fatal("stopping on the budget is logged")
	}
}

func TestTune_RateLimitStopsTheCycle(t *testing.T) {
	h := newHarness(t, failing(llm.ErrRateLimited))
	prev, cur := threeCases()
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("a provider refusal degrades, it does not fail the cycle: %v", err)
	}
	if h.model.callCount() != 1 || len(out.Findings) != 0 {
		t.Fatalf("calls %d findings %d: a 429 stops asking", h.model.callCount(),
			len(out.Findings))
	}
}

func TestTune_MalformedAnswersFeedTheCaseMemory(t *testing.T) {
	h := newHarness(t, answer("not json"), answer("```json\n{oops"), answer(""),
		indexAnswer(t))
	for i := 0; i < 3; i++ {
		out := tune(t, h)
		if len(out.Findings) != 0 {
			t.Fatalf("cycle %d: malformed answers make no findings", i)
		}
	}
	tune(t, h)
	if h.model.callCount() != 3 {
		t.Fatalf("model calls = %d, want 3: the fourth cycle skips the case",
			h.model.callCount())
	}
	if got := h.agent.Stats().ModelCallsSkipped; got != 1 {
		t.Fatalf("model calls skipped = %d, want 1", got)
	}
}

func TestTune_FactsReadErrorDegradesOpen(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.facts.err = errFake
	out := tune(t, h)
	if _, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory); !ok {
		t.Fatal("without facts the proposal is still judged (the gate binds anyway)")
	}
	if !h.logs.contains("facts") {
		t.Fatal("the facts read error is logged")
	}
}

func TestTune_BindingFactRedirectsTheProposal(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.facts.list = []facts.Fact{confirmedFact(12, facts.TypeAppMigrations, facts.KindTable,
		"public.orders", nil)}
	out := tune(t, h)
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok || f.RecommendedSQL != "" || f.Detail["source_fix"] == nil {
		t.Fatalf("finding = %+v: a migration-managed table gets a source-fix packet", f)
	}
	if !strings.Contains(h.model.msgs[0][1].Content, "fact #12") {
		t.Fatal("the model sees the confirmed fact as context")
	}
}

func TestTune_UnsupportedFormsAreDropped(t *testing.T) {
	h := newHarness(t, answer(proposalsJSON(t,
		map[string]any{"type": "raw_sql", "sql": "VACUUM FULL public.orders",
			"evidence": []string{"S1"}, "expected_change_pct": -10},
		map[string]any{"type": "create_statistics", "table": "public.orders",
			"columns": []string{"lower(status)", "customer_id"}, "evidence": []string{"S1"},
			"expected_change_pct": -50})))
	out := tune(t, h)
	if len(out.Findings) != 0 {
		t.Fatalf("findings = %+v", out.Findings)
	}
	if !h.logs.contains(string(ReasonUnsupported)) {
		t.Fatal("rejections are logged with their reason")
	}
}

func TestTune_OpenFindingsReadErrorFailsTheCycle(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.store.openErr = errFake
	prev, cur := ordersPair()
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if !errors.Is(err, errFake) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Contains(out.Failed, optimizer.OptimizerCategory) || h.model.callCount() != 0 {
		t.Fatalf("out %+v calls %d: nothing resolves and nothing is asked", out,
			h.model.callCount())
	}
}

func TestTune_ColdStart(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.indexes.cold = true
	out := tune(t, h)
	if h.model.callCount() != 0 || len(out.Findings) != 0 ||
		!slices.Contains(out.Failed, optimizer.OptimizerCategory) {
		t.Fatalf("cold start: %+v", out)
	}
}

func TestTune_FallbackModel(t *testing.T) {
	h := newHarness(t, failing(errFake))
	fallback := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		indexAnswer(t)}}
	h.agent = New(defaultSettings(), Deps{Model: h.model, Fallback: fallback,
		Indexes: h.indexes, Facts: h.facts, Hints: h.hints, Store: h.store,
		Now: func() time.Time { return t0 }}, h.logs.fn)
	out := tune(t, h)
	if fallback.callCount() != 1 {
		t.Fatalf("fallback calls = %d", fallback.callCount())
	}
	if _, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory); !ok {
		t.Fatal("the fallback's proposal is judged like any other")
	}
}

func TestTune_WithoutAModelOnlyReemits(t *testing.T) {
	h := newHarness(t)
	open := agentFinding("top_statement:101", optimizer.OptimizerCategory, "x")
	h.store.open = []analyzer.Finding{open}
	h.agent = New(defaultSettings(), Deps{Indexes: h.indexes, Facts: h.facts,
		Store: h.store, Now: func() time.Time { return t0 }}, h.logs.fn)
	out := tune(t, h)
	if len(out.Findings) != 1 {
		t.Fatalf("findings = %+v", out.Findings)
	}
}

func TestTune_InvalidInput(t *testing.T) {
	h := newHarness(t)
	if _, err := h.agent.Tune(context.Background(), nil, nil); err == nil {
		t.Fatal("a nil snapshot is an error")
	}
	var nilAgent *Agent
	if _, err := nilAgent.Tune(context.Background(), &collector.Snapshot{}, nil); err == nil {
		t.Fatal("a nil agent is an error")
	}
	if s := nilAgent.Stats(); s != (analyzer.TuningStats{}) {
		t.Fatalf("nil agent stats = %+v", s)
	}
}

func TestTune_StatsWhileTuning(t *testing.T) {
	h := newHarness(t, indexAnswer(t))
	h.indexes.whatIfSkp = 4
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = h.agent.Stats()
			}
		}
	}()
	tune(t, h)
	close(stop)
	wg.Wait()
	if got := h.agent.Stats().WhatIfSkipped; got != 4 {
		t.Fatalf("what-if skips come from the optimizer's memory: %d", got)
	}
}
