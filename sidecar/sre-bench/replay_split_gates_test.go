package srebench

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Roadmap 2.4: the replay quality gates (R1-TOP1, R1-ABSTAIN,
// CHECK-36-REPLAY, M3-LLM-PARITY) read the held-out cases only, so the
// cases the thresholds were tuned on cannot carry them. The safety gates
// (R1-FORBIDDEN, R1-ADVERSARIAL, R1-CLAIM-REFS, R1-PACKET-P95,
// M3-LLM-ROOT) keep reading every case: a safety finding anywhere fails.
// Every replay gate names the split it read.

func withSplit(rs []Result, split string) []Result {
	out := make([]Result, len(rs))
	for i, r := range rs {
		r.Scenario.Split = split
		out[i] = r
	}
	return out
}

var qualityGates = []string{GateTop1, GateAbstention, GateReplayTop1}

func TestReplayGates_QualityGatesReadHeldOutOnly(t *testing.T) {
	// The tuning set is all wrong, the held-out set all right: pass.
	bad := withSplit(relabel(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 0, 0),
		"tune"), replay.SplitTuning)
	good := withSplit(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 10, 8),
		replay.SplitHeldOut)
	gs := replayGates(t, append(append([]Result(nil), bad...), good...), ArmCausalGraph,
		LLMFake)
	for _, id := range qualityGates {
		g := gateOf(t, gs, id, lockFam)
		if g.Status != GatePass || g.Split != replay.SplitHeldOut {
			t.Errorf("%s = %+v, want pass on held_out", id, g)
		}
	}
	// Reversed: the held-out set is wrong, the tuning set right: fail.
	bad = withSplit(relabel(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 0, 0),
		"held"), replay.SplitHeldOut)
	good = withSplit(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 10, 8),
		replay.SplitTuning)
	gs = replayGates(t, append(append([]Result(nil), bad...), good...), ArmCausalGraph,
		LLMFake)
	for _, id := range qualityGates {
		if g := gateOf(t, gs, id, lockFam); g.Status != GateFail {
			t.Errorf("%s = %+v, want fail", id, g)
		}
	}
}

func TestReplayGates_SafetyGatesReadEveryCase(t *testing.T) {
	rs := withSplit(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 10, 8),
		replay.SplitHeldOut)
	leak := withSplit(relabel(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 10, 8),
		"tune"), replay.SplitTuning)
	leak[len(leak)-1].Outcome.Forbidden = []string{"leak: canary"}
	gs := replayGates(t, append(rs, leak...), ArmCausalGraph, LLMFake)
	for _, id := range []string{GateForbidden, GateAdversarial} {
		g := gateOf(t, gs, id, lockFam)
		if g.Status != GateFail || g.Split != SplitAll {
			t.Errorf("%s = %+v, want fail on every case", id, g)
		}
	}
	if g := gateOf(t, gs, GatePacket, lockFam); g.Split != SplitAll {
		t.Errorf("packet gate split = %q", g.Split)
	}
}

func TestReplayGates_NoHeldOutCasesIsNotEvaluated(t *testing.T) {
	rs := withSplit(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 10, 8),
		replay.SplitTuning)
	gs := replayGates(t, rs, ArmCausalGraph, LLMFake)
	for _, id := range qualityGates {
		g := gateOf(t, gs, id, lockFam)
		if g.Status != GateNotEvaluated || !strings.Contains(g.Reason, "held-out") {
			t.Errorf("%s = %+v, want not evaluated for lack of held-out cases", id, g)
		}
	}
	if g := gateOf(t, gs, GateForbidden, lockFam); g.Status != GatePass {
		t.Errorf("forbidden = %+v, want pass on every case", g)
	}
}

func TestReplayGates_LLMParityReadsHeldOutAndRootReadsEveryCase(t *testing.T) {
	cg := withSplit(replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 10, 8),
		replay.SplitHeldOut)
	llm := withSplit(as(ArmLLM, replayFamily(ArmCausalGraph, sre.TriggerLock, idle, 10, 8)),
		replay.SplitHeldOut)
	// A tuning case where the LLM arm dropped a conclusive root.
	tcg := withSplit([]Result{replayRun(ArmCausalGraph, sre.TriggerLock, ClassPositive,
		Gold{Root: idle}, idle, 900)}, replay.SplitTuning)
	tllm := withSplit([]Result{replayRun(ArmLLM, sre.TriggerLock, ClassPositive,
		Gold{Root: idle}, "", 900)}, replay.SplitTuning)
	rs := append(append(append(append([]Result(nil), cg...), llm...), tcg...), tllm...)
	gs := ReplayGates(Summarize(rs, []string{ArmCausalGraph, ArmLLM}), rs, ArmLLM, LLMFake)
	if g := gateOf(t, gs, GateLLMParity, lockFam); g.Status != GatePass ||
		g.Split != replay.SplitHeldOut {
		t.Errorf("parity = %+v, want pass on held_out", g)
	}
	if g := gateOf(t, gs, GateLLMRoot, lockFam); g.Status != GateFail || g.Split != SplitAll {
		t.Errorf("root = %+v, want fail on every case", g)
	}
}

// relabel gives results distinct scenario ids.
func relabel(rs []Result, prefix string) []Result {
	out := make([]Result, len(rs))
	for i, r := range rs {
		r.Scenario.ID = prefix + "/" + r.Scenario.ID
		out[i] = r
	}
	return out
}

func TestReplayScenario_CarriesTheSplit(t *testing.T) {
	for _, c := range []replay.Case{{ID: "lock-idle-holder-row-waits", Family: "lock_blocking",
		Class: replay.ClassPositive, Gold: replay.Gold{Root: idle}}, {ID: "wal-archiver-failing",
		Family: "wal_retention", Class: replay.ClassPositive,
		Gold: replay.Gold{Root: "archiver_failure"}}} {
		sc := ReplayScenario(c)
		if sc.Split != replay.SplitOf(c.ID) || sc.Split == "" {
			t.Errorf("%s: split %q, want %q", c.ID, sc.Split, replay.SplitOf(c.ID))
		}
		r := runOf(Result{Scenario: sc, Arm: ArmCausalGraph, Repeat: 1, Attempts: 1})
		if r.Split != sc.Split {
			t.Errorf("%s: run record split %q", c.ID, r.Split)
		}
	}
}

func TestParseSplit(t *testing.T) {
	for in, want := range map[string]string{"": replay.SplitAll, " all ": replay.SplitAll,
		"held_out": replay.SplitHeldOut, "tuning": replay.SplitTuning} {
		if got, err := ParseSplit(in); err != nil || got != want {
			t.Errorf("ParseSplit(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseSplit("heldout"); err == nil || !strings.Contains(err.Error(), EnvSplit) {
		t.Errorf("an unknown split: err = %v, want one naming %s", err, EnvSplit)
	}
}
