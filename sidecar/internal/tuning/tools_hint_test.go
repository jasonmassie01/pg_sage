package tuning

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// tuning-05: the agent's hinted_explain tool and the hinted EXPLAIN
// evidence behind a hint's prediction.

// explainingHints is a hint sink with the tuner's hinted EXPLAIN.
type explainingHints struct {
	*fakeHints
	enabled  bool
	cmp      tuner.HintComparison
	err      error
	compared []tuner.HintProposal
	// evidence, when set, is the hint_evidence CheckHint adds; noSQL drops
	// the finding's SQL (an unverifiable hint).
	evidence map[string]any
	noSQL    bool
}

func (h *explainingHints) HintComparisonEnabled() bool { return h.enabled }

func (h *explainingHints) HintedExplain(_ context.Context, p tuner.HintProposal) (
	tuner.HintComparison, error) {
	h.compared = append(h.compared, p)
	return h.cmp, h.err
}

func (h *explainingHints) CheckHint(ctx context.Context, p tuner.HintProposal) (
	analyzer.Finding, error) {
	f, err := h.fakeHints.CheckHint(ctx, p)
	if err != nil {
		return f, err
	}
	if h.evidence != nil {
		f.Detail["hint_evidence"] = h.evidence
	}
	if h.noSQL {
		f.RecommendedSQL, f.RollbackSQL = "", ""
	}
	return f, nil
}

func withExplainer(h *harness, enabled bool) *explainingHints {
	eh := &explainingHints{fakeHints: h.hints, enabled: enabled,
		cmp: tuner.HintComparison{Verdict: tuner.HintChanged, Basis: "plan", Reason: "r",
			PlainCost: 100, HintedCost: 104, PlainHash: "v2u:p", HintedHash: "v2u:h",
			PlainPlan:  []byte(`[{"Plan":{"Node Type":"Nested Loop"}}]`),
			HintedPlan: []byte(`[{"Plan":{"Node Type":"Hash Join"}}]`)}}
	h.agent.deps.Hints = eh
	return eh
}

func hasTool(a *Agent, name string) bool {
	for _, s := range a.toolSpecs() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// State: the tool is offered only with an enabled hinted EXPLAIN.
func TestHintExplainTool_OfferedOnlyWhenEnabled(t *testing.T) {
	h := newHarness(t)
	if hasTool(h.agent, "hinted_explain") {
		t.Fatal("offered without a hinted EXPLAIN")
	}
	withExplainer(h, false)
	if hasTool(h.agent, "hinted_explain") {
		t.Fatal("offered with the gate off")
	}
	withExplainer(h, true)
	if !hasTool(h.agent, "hinted_explain") {
		t.Fatal("not offered with the gate on")
	}
}

// Happy path: the result is evidence cited as hint_explain:<qid>:<hash>,
// planned for the case statement's text.
func TestHintExplainTool_RegistersEvidence(t *testing.T) {
	h := newHarness(t)
	eh := withExplainer(h, true)
	tb := ordersToolbox(t, h)
	qid := tb.c.Statements[0].QueryID
	out := call(t, tb, "hinted_explain", `{"queryid":"`+itoa(qid)+`","hint":"HashJoin(c o)"}`)
	if out["error"] != nil || out["evidence_id"] != "R1" || out["verdict"] != "changed" ||
		out["plain_cost"] != 100.0 || out["hinted_cost"] != 104.0 ||
		!strings.Contains(out["hinted_plan"].(string), "Hash Join") {
		t.Fatalf("result %v", out)
	}
	ev := tb.evidence["R1"]
	if ev.Kind != "tool:hinted_explain" || ev.Ref != "hint_explain:"+itoa(qid)+":v2u:h" {
		t.Fatalf("evidence %+v", ev)
	}
	if len(eh.compared) != 1 || eh.compared[0].Query != tb.c.Statements[0].Text ||
		eh.compared[0].Hint != "HashJoin(c o)" || eh.compared[0].QueryID != qid {
		t.Fatalf("compared %+v", eh.compared)
	}
}

// Invalid input, error propagation, boundary: an out-of-case queryid, a
// refused hint and a fourth call per case are errors that register nothing.
func TestHintExplainTool_Refusals(t *testing.T) {
	h := newHarness(t)
	eh := withExplainer(h, true)
	tb := ordersToolbox(t, h)
	qid := itoa(tb.c.Statements[0].QueryID)
	out := call(t, tb, "hinted_explain", `{"queryid":"999999","hint":"SeqScan(o)"}`)
	if msg, _ := out["error"].(string); !strings.Contains(msg, "not a statement") {
		t.Fatalf("out of case: %v", out)
	}
	eh.err = errors.New("invalid query hint: x")
	out = call(t, tb, "hinted_explain", `{"queryid":"`+qid+`","hint":"x"}`)
	if msg, _ := out["error"].(string); !strings.Contains(msg, "invalid query hint") {
		t.Fatalf("refused hint: %v", out)
	}
	eh.err = nil
	for i := 0; i < maxHintExplainPerCase-1; i++ {
		args := `{"queryid":"` + qid + `","hint":"SeqScan(o)"}`
		if out := call(t, tb, "hinted_explain", args); out["error"] != nil {
			t.Fatalf("call %d within budget: %v", i, out)
		}
	}
	out = call(t, tb, "hinted_explain", `{"queryid":"`+qid+`","hint":"SeqScan(o)"}`)
	if msg, _ := out["error"].(string); !strings.Contains(msg, "at most 3") {
		t.Fatalf("over budget: %v", out)
	}
	if len(tb.evidence) != maxHintExplainPerCase-1 {
		t.Fatalf("evidence %d, want %d", len(tb.evidence), maxHintExplainPerCase-1)
	}
}

// The prediction rests on the plan comparison when the hinted EXPLAIN proved
// the plan changes; an unverifiable hint is admitted advisory and never
// recorded with the tuner.
func TestHintExplainJudge_PredictionNoteAndAdvisory(t *testing.T) {
	h := newHarness(t)
	eh := withExplainer(h, true)
	eh.evidence = map[string]any{"verdict": "changed"}
	j := judgeOne(t, h, nil, hintProposal(101))
	if j.Verdict != VerdictAdmitted || j.Prediction.Note != "plan cost comparison" ||
		j.Hint == nil {
		t.Fatalf("changed: %+v", j)
	}
	eh.evidence = map[string]any{"verdict": "unverifiable"}
	eh.noSQL = true
	j = judgeOne(t, h, nil, hintProposal(101))
	if j.Verdict != VerdictAdmitted || j.Prediction.Note != "the model's estimate" ||
		j.Hint != nil || j.Finding.RecommendedSQL != "" {
		t.Fatalf("unverifiable: %+v", j)
	}
	if got := h.agent.recordHints(context.Background(), []Judged{j}); len(got) != 1 ||
		len(h.hints.recorded) != 0 {
		t.Fatalf("an advisory hint was recorded: %d findings, %v", len(got), h.hints.recorded)
	}
	eh.evidence, eh.noSQL = nil, false
	if j := judgeOne(t, h, nil, hintProposal(101)); j.Prediction.Note != "the model's estimate" {
		t.Fatalf("no evidence: note %q", j.Prediction.Note)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
