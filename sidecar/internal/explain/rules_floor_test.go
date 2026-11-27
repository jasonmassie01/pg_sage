package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// tuning-10: deterministic rules are the floor of a natural-language
// EXPLAIN. Without an LLM the rules write the lists and the summary; with
// one the model may add entries and write the summary, never remove a
// rule entry.

const spillingPlan = `[{"Plan": {"Node Type": "Hash Join", "Total Cost": 900,
	"Plan Rows": 1000, "Actual Rows": 1000, "Actual Total Time": 40, "Actual Loops": 1,
	"Plans": [{"Node Type": "Sort", "Parent Relationship": "Outer", "Total Cost": 500,
		"Plan Rows": 1000, "Actual Rows": 1000, "Actual Total Time": 30, "Actual Loops": 1,
		"Sort Method": "external merge", "Sort Space Used": 8396, "Sort Space Type": "Disk",
		"Plans": [{"Node Type": "Seq Scan", "Relation Name": "orders",
			"Parent Relationship": "Outer", "Total Cost": 100, "Plan Rows": 1000,
			"Actual Rows": 1000, "Actual Total Time": 5, "Actual Loops": 1}]},
	{"Node Type": "Hash", "Parent Relationship": "Inner", "Total Cost": 50,
		"Plan Rows": 10, "Actual Rows": 10, "Actual Total Time": 1, "Actual Loops": 1,
		"Plans": [{"Node Type": "Seq Scan", "Relation Name": "customers",
			"Total Cost": 20, "Plan Rows": 10, "Actual Rows": 10, "Actual Total Time": 0.5,
			"Actual Loops": 1}]}]},
	"Planning Time": 0.3, "Execution Time": 41.5}]`

const quietPlan = `[{"Plan": {"Node Type": "Index Scan", "Relation Name": "orders",
	"Total Cost": 8, "Plan Rows": 1, "Actual Rows": 1, "Actual Total Time": 0.02,
	"Actual Loops": 1}, "Execution Time": 0.05}]`

func floorResult(t *testing.T, ex *Explainer, plan string, analyzed bool) *ExplainResult {
	t.Helper()
	r := buildResult("SELECT * FROM orders JOIN customers USING (id) ORDER BY 1",
		json.RawMessage(plan), analyzed)
	ex.applyRules(context.Background(), r, analyzed)
	return r
}

func ruleInsights(r *ExplainResult, kind string) []Insight {
	var out []Insight
	for _, in := range r.Insights {
		if in.Kind == kind && in.Source == SourceRules {
			out = append(out, in)
		}
	}
	return out
}

func TestRulesFloor_DiskSortNamesSpill(t *testing.T) {
	r := floorResult(t, New(nil, &config.ExplainConfig{}, noopLogFn), spillingPlan, true)
	if len(r.SlowBecause) == 0 || !strings.Contains(r.SlowBecause[0], "spilled 8.2 MB to disk") {
		t.Fatalf("SlowBecause = %v, want the 8.2 MB disk spill", r.SlowBecause)
	}
	got := ruleInsights(r, InsightSlowBecause)
	if len(got) != 1 || got[0].Symptom != "disk_sort" || got[0].Text != r.SlowBecause[0] {
		t.Errorf("rule insights = %+v, want one disk_sort entry tagged rules", got)
	}
	if len(r.Recommendations) == 0 || !strings.Contains(r.Recommendations[0], "work_mem") {
		t.Errorf("Recommendations = %v, want a work_mem recommendation", r.Recommendations)
	}
	if len(ruleInsights(r, InsightRecommendation)) != len(r.Recommendations) {
		t.Errorf("every recommendation needs a rules insight: %+v", r.Insights)
	}
	if r.Summary != "Hash Join; sort spilled 8.2 MB to disk" {
		t.Errorf("Summary = %q, want %q", r.Summary, "Hash Join; sort spilled 8.2 MB to disk")
	}
}

func TestRulesFloor_QuietPlanSaysNoProblem(t *testing.T) {
	r := floorResult(t, New(nil, &config.ExplainConfig{}, noopLogFn), quietPlan, true)
	if len(r.SlowBecause) != 0 || len(r.Recommendations) != 0 || len(r.Insights) != 0 {
		t.Errorf("lists = %v / %v / %v, want empty", r.SlowBecause, r.Recommendations,
			r.Insights)
	}
	if r.SlowBecause == nil || r.Recommendations == nil || r.Insights == nil {
		t.Error("lists must be empty slices, not null, in the JSON")
	}
	if strings.Contains(r.Summary, "LLM") || !strings.HasPrefix(r.Summary, "Index Scan on orders") {
		t.Errorf("Summary = %q, want a rules summary of the root node", r.Summary)
	}
}

// A plan-only plan never fires the rules that need actuals, even when it
// carries leftover spill fields.
func TestRulesFloor_EstimatesOnlySkipsActualsRules(t *testing.T) {
	r := floorResult(t, New(nil, &config.ExplainConfig{}, noopLogFn), spillingPlan, false)
	for _, s := range r.SlowBecause {
		if strings.Contains(s, "spilled") {
			t.Errorf("SlowBecause = %v: a plan without ANALYZE cannot show a spill", r.SlowBecause)
		}
	}
}

// Invalid input: an unreadable plan leaves empty lists and says so.
func TestRulesFloor_InvalidPlan(t *testing.T) {
	r := floorResult(t, New(nil, &config.ExplainConfig{}, noopLogFn), `not json`, true)
	if len(r.SlowBecause) != 0 || r.Summary == "" {
		t.Errorf("SlowBecause=%v Summary=%q, want no entries and a summary", r.SlowBecause,
			r.Summary)
	}
}

// Nil/empty plan.
func TestRulesFloor_EmptyPlan(t *testing.T) {
	r := floorResult(t, New(nil, &config.ExplainConfig{}, noopLogFn), `[]`, false)
	if r.SlowBecause == nil || len(r.SlowBecause) != 0 || r.Summary == "" {
		t.Errorf("SlowBecause=%v Summary=%q", r.SlowBecause, r.Summary)
	}
}

func llmExplainer(t *testing.T, content string, status int) *Explainer {
	t.Helper()
	srv := newTestLLMServer(t, content, status)
	t.Cleanup(srv.Close)
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 5}, noopLogFn)
	return NewWithLLM(nil, &config.ExplainConfig{}, client, noopLogFn)
}

// Error propagation: an LLM error keeps the rule entries and summary.
func TestRulesFloor_LLMErrorKeepsRules(t *testing.T) {
	ex := llmExplainer(t, "", http.StatusInternalServerError)
	r := floorResult(t, ex, spillingPlan, true)
	before := append([]string(nil), r.SlowBecause...)
	if degraded := ex.enhanceWithLLM(context.Background(), r); !degraded {
		t.Error("an LLM error must report degraded")
	}
	if strings.Join(r.SlowBecause, "|") != strings.Join(before, "|") || len(before) == 0 {
		t.Errorf("SlowBecause = %v, want the rule entries %v kept", r.SlowBecause, before)
	}
	if r.Summary != "Hash Join; sort spilled 8.2 MB to disk" {
		t.Errorf("Summary = %q, want the rules summary kept", r.Summary)
	}
}

// Error propagation: a reply with empty lists keeps the rule entries.
func TestRulesFloor_LLMEmptyListsKeepRules(t *testing.T) {
	ex := llmExplainer(t, `{"summary":"Joins orders to customers.","slow_because":[],`+
		`"recommendations":[]}`, http.StatusOK)
	r := floorResult(t, ex, spillingPlan, true)
	ex.enhanceWithLLM(context.Background(), r)
	if len(ruleInsights(r, InsightSlowBecause)) != 1 || len(r.SlowBecause) != 1 {
		t.Errorf("SlowBecause = %v insights = %+v, want the rule entry kept", r.SlowBecause,
			r.Insights)
	}
	if len(r.Recommendations) == 0 {
		t.Error("rule recommendations were removed")
	}
	if r.Summary != "Joins orders to customers." {
		t.Errorf("Summary = %q, want the model's summary", r.Summary)
	}
}

// The model adds entries after the rules, tagged llm, and a restated rule
// entry is not duplicated.
func TestRulesFloor_LLMAddsDeduplicated(t *testing.T) {
	ex := llmExplainer(t, `{"summary":"s","slow_because":["Sort spilled 8.2 MB to disk.",`+
		`"the hash join builds on customers"],"recommendations":["add an index on orders(id)"]}`,
		http.StatusOK)
	r := floorResult(t, ex, spillingPlan, true)
	rules := len(r.Recommendations)
	ex.enhanceWithLLM(context.Background(), r)
	if len(r.SlowBecause) != 2 || r.SlowBecause[1] != "the hash join builds on customers" {
		t.Fatalf("SlowBecause = %v, want the rule entry then one new model entry", r.SlowBecause)
	}
	if !strings.Contains(r.SlowBecause[0], "spilled 8.2 MB") {
		t.Errorf("SlowBecause[0] = %q, want the rule entry first", r.SlowBecause[0])
	}
	if len(r.Recommendations) != rules+1 {
		t.Errorf("Recommendations = %v, want %d rule entries plus one", r.Recommendations, rules)
	}
	var llmCount int
	for _, in := range r.Insights {
		if in.Source == SourceLLM {
			llmCount++
		}
	}
	if llmCount != 2 {
		t.Errorf("insights = %+v, want 2 tagged llm", r.Insights)
	}
}

// The model's reply cannot remove a rule entry by any shape: null lists,
// a markdown-wrapped reply, or an unparsable one.
func TestRulesFloor_LLMReplyShapesNeverRemoveRules(t *testing.T) {
	for _, reply := range []string{
		`{"summary":"x","slow_because":null,"recommendations":null}`,
		"```json\n{\"summary\":\"x\",\"slow_because\":[]}\n```",
		`not json at all`,
		``,
	} {
		ex := llmExplainer(t, reply, http.StatusOK)
		r := floorResult(t, ex, spillingPlan, true)
		ex.enhanceWithLLM(context.Background(), r)
		if len(ruleInsights(r, InsightSlowBecause)) != 1 || len(r.SlowBecause) != 1 {
			t.Errorf("reply %q: SlowBecause = %v, want the rule entry kept", reply, r.SlowBecause)
		}
	}
}

// Concurrent access: one Explainer serves parallel floors and LLM merges;
// each result keeps its own entries.
func TestRulesFloor_ConcurrentOnOneExplainer(t *testing.T) {
	ex := llmExplainer(t, `{"summary":"s","slow_because":["model reason"]}`, http.StatusOK)
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plan := spillingPlan
			if i%2 == 1 {
				plan = quietPlan
			}
			r := buildResult(fmt.Sprintf("SELECT %d", i), json.RawMessage(plan), true)
			ex.applyRules(context.Background(), r, true)
			ex.enhanceWithLLM(context.Background(), r)
			want := 2
			if i%2 == 1 {
				want = 1
			}
			if len(r.SlowBecause) != want {
				errs <- fmt.Sprintf("call %d: SlowBecause = %v, want %d", i, r.SlowBecause, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// EstimatedPlan (the agent path) gets the same rules floor, estimates only.
func TestEstimatedPlan_HasRulesSummary(t *testing.T) {
	r := EstimatedPlan("SELECT 1", json.RawMessage(quietPlan))
	if r.SlowBecause == nil || strings.Contains(r.Summary, "LLM analysis pending") {
		t.Errorf("SlowBecause=%v Summary=%q, want a rules floor", r.SlowBecause, r.Summary)
	}
}
