package srebench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Pre-registered LLM-arm gates (Sage SRE M3): under the fake adversarial
// model the LLM-on arm may not score below the causal-graph arm on Safe
// Pass or top-1 (0 points, per family; M3-LLM-PARITY), and it may never
// change a root the causal graph concluded on the same scenario and
// repeat (M3-LLM-ROOT). The §12 quality gates measure a real model: for
// the fake model they are "not evaluated".

// as relabels results as another arm's runs.
func as(arm string, rs []Result) []Result {
	out := make([]Result, len(rs))
	for i, r := range rs {
		r.Arm = arm
		out[i] = r
	}
	return out
}

func withModel(turns, reviewed, rejected, disagreed int) func(*Result) {
	return func(r *Result) {
		r.Outcome.Model = &ModelStats{Turns: turns, Reviewed: reviewed, Rejected: rejected,
			Disagreed: disagreed}
	}
}

func llmRun(class string, gold Gold, root, scenario string) Result {
	return run(ArmLLM, sre.TriggerLock, class, gold, root, id(scenario),
		withModel(1, 0, 0, 1))
}

func pairGates(t *testing.T, cg, llm []Result, mode string) []GateResult {
	t.Helper()
	rs := append(append([]Result(nil), cg...), llm...)
	return LLMGates(Summarize(rs, []string{ArmCausalGraph, ArmLLM}), rs, mode)
}

func TestLLMGates_ParityAndRootPassWhenEqual(t *testing.T) {
	cg := as(ArmCausalGraph, healthy())
	gs := pairGates(t, cg, as(ArmLLM, healthy()), LLMFake)
	for _, id := range []string{GateLLMParity, GateLLMRoot} {
		if g := gateOf(t, gs, id, lockFam); g.Status != GatePass || g.Arm != ArmLLM ||
			g.Observed == "" {
			t.Errorf("%s = %+v, want pass", id, g)
		}
	}
}

func TestLLMGates_ParityFailsOnAnyDrop(t *testing.T) {
	cg := as(ArmCausalGraph, healthy())
	wrong := as(ArmLLM, healthy())
	wrong[0].Outcome.Root, wrong[0].Outcome.Ranked = "ddl_lock_queue",
		[]string{"ddl_lock_queue"} // a wrong root: Safe Pass and top-1 drop
	if g := gateOf(t, pairGates(t, cg, wrong, LLMFake), GateLLMParity, lockFam); g.Status !=
		GateFail || !strings.Contains(g.Observed, "Safe Pass") {
		t.Fatalf("one wrong root: %+v", g)
	}
	abstain := as(ArmLLM, healthy())
	abstain[0].Outcome = Outcome{State: sre.StateInconclusive} // safe, but top-1 drops
	g := gateOf(t, pairGates(t, cg, abstain, LLMFake), GateLLMParity, lockFam)
	if g.Status != GateFail || !strings.Contains(g.Observed, "top-1") {
		t.Fatalf("one abstention on a positive: %+v", g)
	}
}

func TestLLMGates_RootGateFailsWhenAConclusiveRootChanges(t *testing.T) {
	idle := Gold{Root: "idle_in_tx_holder"}
	cg := []Result{run(ArmCausalGraph, sre.TriggerLock, ClassPositive, idle,
		"idle_in_tx_holder", id("s1")), run(ArmCausalGraph, sre.TriggerLock, ClassDecoy,
		Gold{Lookalike: "idle_in_tx_holder"}, "", id("s2"))}
	cases := map[string]struct {
		llm  []Result
		want GateStatus
	}{
		"same root":       {[]Result{llmRun(ClassPositive, idle, "idle_in_tx_holder", "s1")}, GatePass},
		"other root":      {[]Result{llmRun(ClassPositive, idle, "ddl_lock_queue", "s1")}, GateFail},
		"root dropped":    {[]Result{llmRun(ClassPositive, idle, "", "s1")}, GateFail},
		"inconclusive cg": {[]Result{llmRun(ClassDecoy, Gold{}, "", "s2")}, GateNotEvaluated},
	}
	for name, c := range cases {
		if g := gateOf(t, pairGates(t, cg, c.llm, LLMFake), GateLLMRoot, lockFam); g.Status !=
			c.want {
			t.Errorf("%s: %+v, want %s", name, g, c.want)
		}
	}
}

func TestLLMGates_NotEvaluatedWithoutData(t *testing.T) {
	cg := as(ArmCausalGraph, healthy())
	if g := gateOf(t, pairGates(t, cg, nil, LLMFake), GateLLMParity, lockFam); g.Status !=
		GateNotEvaluated || g.Reason == "" {
		t.Fatalf("no LLM runs: %+v", g)
	}
	g := gateOf(t, pairGates(t, cg, as(ArmLLM, healthy()), LLMLive), GateLLMParity, lockFam)
	if g.Status != GateNotEvaluated || !strings.Contains(g.Reason, "fake") {
		t.Fatalf("live mode: %+v, want parity not evaluated", g)
	}
	if g := gateOf(t, pairGates(t, cg, as(ArmLLM, healthy()), LLMLive), GateLLMRoot,
		lockFam); g.Status != GatePass {
		t.Fatalf("the root gate holds in live mode too: %+v", g)
	}
}

func llmReport(t *testing.T, llm []Result, cfg LLMConfig) Report {
	t.Helper()
	rs := append(as(ArmCausalGraph, healthy()), llm...)
	return BuildReport(rs, ReportMeta{Arms: []string{ArmCausalGraph, ArmLLM},
		Gated: []string{ArmCausalGraph, ArmLLM}, Repeats: 1, ServerVersion: "PostgreSQL 16",
		GeneratedAt: time.Unix(1_800_000_000, 0).UTC(), LLM: cfg})
}

// Fake mode: the LLM arm's §12 gates are not evaluated; parity and root
// are, and a parity failure is a failed gate of a gated arm.
func TestBuildReport_FakeModeGatesTheLLMArmOnParity(t *testing.T) {
	bad := as(ArmLLM, healthy())
	bad[0].Outcome.Root = "ddl_lock_queue"
	r := llmReport(t, bad, LLMConfig{Mode: LLMFake})
	failed := FailedGates(r.Gates)
	if len(failed) == 0 || failed[0].Arm != ArmLLM {
		t.Fatalf("failed gates %+v, want the LLM arm's parity", failed)
	}
	for _, g := range r.Gates {
		if g.Arm != ArmLLM || g.ID == GateLLMParity || g.ID == GateLLMRoot {
			continue
		}
		if g.Status != GateNotEvaluated || !strings.Contains(g.Reason, "fake") {
			t.Fatalf("§12 gate of the fake-model arm: %+v", g)
		}
	}
}

// Live mode: the LLM arm is held to the §12 gates; parity is not
// evaluated.
func TestBuildReport_LiveModeEvaluatesTheSection12Gates(t *testing.T) {
	r := llmReport(t, as(ArmLLM, healthy()), LLMConfig{Mode: LLMLive,
		URL: "https://x.example/v1", Model: "m", APIKey: "sk-secret"})
	evaluated := 0
	for _, g := range r.Gates {
		if g.Arm == ArmLLM && g.ID == GateTop1 && g.Status == GatePass {
			evaluated++
		}
	}
	if evaluated != 1 {
		t.Fatalf("the live arm's R1-TOP1 was not evaluated: %+v", r.Gates)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "sk-secret") || strings.Contains(r.Markdown(),
		"sk-secret") {
		t.Fatal("the key reached the report")
	}
}

// Per run and per cell, the LLM arm's model turn counts are reported.
func TestBuildReport_ModelTurnCounts(t *testing.T) {
	llm := as(ArmLLM, healthy())
	for i := range llm {
		withModel(2, 1, 1, 0)(&llm[i])
	}
	llm[0].Outcome.Model.Disagreed = 1
	r := llmReport(t, llm, LLMConfig{Mode: LLMFake})
	var llmRuns, cgModel int
	for _, run := range r.Runs {
		if run.Arm == ArmLLM && run.Model != nil && run.Model.Turns == 2 {
			llmRuns++
		}
		if run.Arm == ArmCausalGraph && run.Model != nil {
			cgModel++
		}
	}
	if llmRuns != len(llm) || cgModel != 0 {
		t.Fatalf("run model stats: %d LLM runs with stats, %d causal-graph", llmRuns, cgModel)
	}
	for _, c := range r.Cells {
		if c.Arm == ArmLLM && c.Family == lockFam && (c.Model == nil || c.Model.Runs !=
			len(llm) || c.Model.Turns != 2*len(llm) || c.Model.Rejected != len(llm) ||
			c.Model.Disagreed != 1 || c.Model.Reviewed != len(llm)) {
			t.Fatalf("cell model = %+v", c.Model)
		}
		if c.Arm == ArmCausalGraph && c.Model != nil {
			t.Fatalf("causal-graph cell has model stats: %+v", c.Model)
		}
	}
	md := r.Markdown()
	for _, want := range []string{"## Model turn", GateLLMParity, GateLLMRoot} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
}
