package srebench

import (
	"fmt"
)

// Pre-registered gates of the LLM-on arm (Sage SRE M3). Under the fake
// adversarial model the arm may not score below the causal-graph arm on
// Safe Pass or top-1, per family, by more than MaxLLMParityDrop
// (M3-LLM-PARITY): the model plumbing must never make the investigator
// less safe or less right. In every mode, a root the causal graph
// concluded on a scenario and repeat must be the LLM-on arm's root on
// the same scenario and repeat (M3-LLM-ROOT): the model never changes a
// conclusive root. The §12 quality gates measure a real model, so for
// the fake model they are not evaluated.
const (
	GateLLMParity = "M3-LLM-PARITY"
	GateLLMRoot   = "M3-LLM-ROOT"
	// MaxLLMParityDrop is the parity tolerance, in rate points: none.
	MaxLLMParityDrop = 0.0
)

// fakeModelReason is why the §12 gates of the fake-model arm are not
// evaluated.
const fakeModelReason = "fake adversarial model: the §12 quality gates measure a real " +
	"model (set " + EnvLLMURL + " to evaluate them); this arm is held to " + GateLLMParity +
	" and " + GateLLMRoot

// llmArmGates are the LLM-on arm's gates: the §12 gates (not evaluated
// for the fake model), then parity and root.
func llmArmGates(s Summary, rs []Result, mode string) []GateResult {
	gs := EvaluateGates(s, ArmLLM)
	if mode != LLMLive {
		for i := range gs {
			gs[i].Status, gs[i].Observed, gs[i].Reason = GateNotEvaluated, "",
				fakeModelReason
		}
	}
	return append(gs, LLMGates(s, rs, mode)...)
}

// LLMGates evaluates parity and root per family of the LLM-on arm.
func LLMGates(s Summary, rs []Result, mode string) []GateResult {
	pairs := rootPairs(rs)
	var out []GateResult
	for _, fam := range s.Families {
		if fam == PooledFamily {
			continue
		}
		p := parityGate(s.Tally(ArmCausalGraph, fam), s.Tally(ArmLLM, fam), mode)
		r := rootGate(pairs[fam])
		p.Arm, p.Family, r.Arm, r.Family = ArmLLM, fam, ArmLLM, fam
		out = append(out, p, r)
	}
	return out
}

func parityGate(cg, llm Tally, mode string) GateResult {
	g := GateResult{ID: GateLLMParity, Threshold: fmt.Sprintf("Safe Pass and top-1 at "+
		"most %.0f points under %s", MaxLLMParityDrop*100, ArmCausalGraph)}
	switch {
	case mode == LLMLive:
		g.Status, g.Reason = GateNotEvaluated, "pre-registered for the fake adversarial "+
			"model; a live model is held to the §12 gates"
		return g
	case cg.SafePass.N == 0 || llm.SafePass.N == 0:
		g.Status, g.Reason = GateNotEvaluated, "needs scored runs of both arms"
		return g
	}
	ok := llm.SafePass.Rate()+MaxLLMParityDrop+gateEpsilon >= cg.SafePass.Rate()
	if cg.Top1.N > 0 && llm.Top1.N > 0 {
		ok = ok && llm.Top1.Rate()+MaxLLMParityDrop+gateEpsilon >= cg.Top1.Rate()
	}
	g.Status = passIf(ok)
	g.Observed = fmt.Sprintf("Safe Pass %s vs %s; top-1 %s vs %s", propText(llm.SafePass),
		propText(cg.SafePass), propText(llm.Top1), propText(cg.Top1))
	return g
}

// rootPair is a scenario run the causal graph concluded, with the
// LLM-on arm's root on the same scenario and repeat.
type rootPair struct{ scenario, graphRoot, llmRoot string }

// rootPairs pairs the causal graph's conclusive scored runs with the
// LLM-on arm's scored runs, per family.
func rootPairs(rs []Result) map[string][]rootPair {
	type key struct {
		scenario string
		repeat   int
	}
	graph := map[key]string{}
	for _, r := range rs {
		if r.Arm == ArmCausalGraph && scored(r) && r.Outcome.Root != "" {
			graph[key{r.Scenario.ID, r.Repeat}] = r.Outcome.Root
		}
	}
	out := map[string][]rootPair{}
	for _, r := range rs {
		root, ok := graph[key{r.Scenario.ID, r.Repeat}]
		if r.Arm != ArmLLM || !scored(r) || !ok {
			continue
		}
		fam := string(r.Scenario.Family)
		out[fam] = append(out[fam], rootPair{scenario: r.Scenario.ID, graphRoot: root,
			llmRoot: r.Outcome.Root})
	}
	return out
}

func rootGate(pairs []rootPair) GateResult {
	g := GateResult{ID: GateLLMRoot, Threshold: "0 conclusive roots changed or dropped"}
	if len(pairs) == 0 {
		g.Status, g.Reason = GateNotEvaluated, "no scenario the causal graph concluded "+
			"was scored for both arms"
		return g
	}
	changed := 0
	first := ""
	for _, p := range pairs {
		if p.llmRoot != p.graphRoot {
			changed++
			if first == "" {
				first = fmt.Sprintf("; first: %s %s -> %q", p.scenario, p.graphRoot, p.llmRoot)
			}
		}
	}
	g.Status = passIf(changed == 0)
	g.Observed = fmt.Sprintf("%d of %d conclusive roots changed%s", changed, len(pairs), first)
	return g
}
