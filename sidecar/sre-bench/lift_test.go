package srebench

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/modellift"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Roadmap 2.4 "measure the model": per family and arm, on held-out replay
// cases only, the bench scores the model against the deterministic
// causal graph:
//   - override precision: of the runs where the model ranked another
//     open hypothesis above the graph's conclusive root, how many named
//     the gold root (Wilson interval beside it);
//   - inconclusive-case lift: of the cases the causal graph alone left
//     inconclusive (same case, same repeat), how many the model resolved
//     correctly (through its probe, or its top-ranked hypothesis) minus
//     how many it resolved wrongly;
//   - Safe Pass of both arms, and Safe Pass had the overrides been
//     adopted, which the override rule holds against the graph's.

// No concurrent access tests: BuildModelLift is a pure function of its
// results.

func heldOut(r Result) Result {
	r.Scenario.Split = replay.SplitHeldOut
	return r
}

func tuning(r Result) Result {
	r.Scenario.Split = replay.SplitTuning
	return r
}

// pair is one held-out case run by both arms: the causal graph's root,
// the LLM arm's stored root and its model stats.
func pair(fam sre.TriggerKind, scenario, class string, gold Gold, graphRoot,
	llmRoot string, m ModelStats) []Result {
	cg := heldOut(run(ArmCausalGraph, fam, class, gold, graphRoot, id(scenario)))
	stats := m
	llm := heldOut(run(ArmLLM, fam, class, gold, llmRoot, id(scenario),
		func(r *Result) { r.Outcome.Model = &stats }))
	return []Result{cg, llm}
}

func disagreed(graph, model string) ModelStats {
	return ModelStats{Turns: 1, Disagreed: 1, GraphRoot: graph, ModelRoot: model,
		Authority: sre.ContestAdvisory}
}

func liftFor(t *testing.T, recs []LiftRecord, fam string) LiftRecord {
	t.Helper()
	for _, r := range recs {
		if r.Family == fam && r.Arm == ArmLLM {
			return r
		}
	}
	t.Fatalf("no %s lift record for %s in %+v", ArmLLM, fam, recs)
	return LiftRecord{}
}

const idle, ddl, prepared = "idle_in_tx_holder", "ddl_lock_queue", "prepared_xact_holder"

// overrideMix: 3 right overrides (graph wrong), 2 wrong ones (graph
// right), 5 agreements.
func overrideMix() []Result {
	var rs []Result
	gold := Gold{Root: idle}
	for i := 0; i < 3; i++ {
		rs = append(rs, pair(sre.TriggerLock, "right-"+string(rune('a'+i)), ClassPositive,
			gold, ddl, ddl, disagreed(ddl, idle))...)
	}
	for i := 0; i < 2; i++ {
		rs = append(rs, pair(sre.TriggerLock, "wrong-"+string(rune('a'+i)), ClassPositive,
			gold, idle, idle, disagreed(idle, prepared))...)
	}
	for i := 0; i < 5; i++ {
		rs = append(rs, pair(sre.TriggerLock, "agree-"+string(rune('a'+i)), ClassPositive,
			gold, idle, idle, ModelStats{Turns: 1, Reviewed: 1, RankedFirst: idle})...)
	}
	return rs
}

func TestModelLift_OverridePrecisionAndAdoptedSafePass(t *testing.T) {
	recs := BuildModelLift(overrideMix(), LLMLive, false)
	r := liftFor(t, recs, lockFam)
	if r.Baseline != ArmCausalGraph || r.Split != replay.SplitHeldOut || r.Mode != LLMLive ||
		r.Runs != 10 {
		t.Fatalf("record header = %+v", r)
	}
	if r.Overrides.K != 3 || r.Overrides.N != 5 || r.Overrides.Low == nil ||
		!near(*r.Overrides.Low, 0.23072) {
		t.Fatalf("override precision = %+v", r.Overrides)
	}
	if r.SafePass.K != 7 || r.SafePass.N != 10 || r.BaselineSafePass.K != 7 ||
		r.BaselineSafePass.N != 10 || r.SafePassLift == nil || *r.SafePassLift != 0 {
		t.Fatalf("Safe Pass %+v vs %+v, lift %v", r.SafePass, r.BaselineSafePass,
			r.SafePassLift)
	}
	if r.OverrideSafePass.K != 8 || r.OverrideSafePass.N != 10 {
		t.Fatalf("Safe Pass with overrides adopted = %+v, want 8/10", r.OverrideSafePass)
	}
	if r.Inconclusive != 0 || r.InconclusiveLift != 0 {
		t.Fatalf("no inconclusive case in the mix: %+v", r)
	}
	if r.OverrideRule.Eligible || !strings.Contains(r.OverrideRule.Reason, "overrides") {
		t.Fatalf("3/5 must stay advisory: %+v", r.OverrideRule)
	}
}

func TestModelLift_ZeroOverrides(t *testing.T) {
	gold := Gold{Root: idle}
	rs := pair(sre.TriggerLock, "s1", ClassPositive, gold, idle, idle,
		ModelStats{Turns: 1, Reviewed: 1, RankedFirst: idle})
	r := liftFor(t, BuildModelLift(rs, LLMLive, false), lockFam)
	if r.Overrides.N != 0 || r.Overrides.Rate != nil || r.OverrideRule.Eligible ||
		!strings.Contains(r.OverrideRule.Reason, "no overrides") {
		t.Fatalf("zero overrides = %+v", r)
	}
	if r.OverrideSafePass.K != r.SafePass.K || r.OverrideSafePass.N != r.SafePass.N {
		t.Fatalf("without overrides the adopted Safe Pass equals the arm's: %+v vs %+v",
			r.OverrideSafePass, r.SafePass)
	}
}

func TestModelLift_OverrideOnAnInsufficientCaseIsWrong(t *testing.T) {
	decoy := Gold{Lookalike: idle}
	rs := pair(sre.TriggerLock, "decoy", ClassDecoy, decoy, idle, idle, disagreed(idle, ddl))
	r := liftFor(t, BuildModelLift(rs, LLMLive, false), lockFam)
	if r.Overrides.K != 0 || r.Overrides.N != 1 || r.OverrideSafePass.K != 0 {
		t.Fatalf("an override on a decoy = %+v", r)
	}
}

func TestModelLift_AllOverridesRightAndEligible(t *testing.T) {
	var rs []Result
	for i := 0; i < 16; i++ {
		rs = append(rs, pair(sre.TriggerLock, "r"+string(rune('a'+i)), ClassPositive,
			Gold{Root: idle}, ddl, ddl, disagreed(ddl, idle))...)
	}
	r := liftFor(t, BuildModelLift(rs, LLMLive, false), lockFam)
	if r.Overrides.K != 16 || r.Overrides.N != 16 || !r.OverrideRule.Eligible ||
		r.OverrideSafePass.K != 16 || r.BaselineSafePass.K != 0 {
		t.Fatalf("16/16 = %+v", r)
	}
	if r := liftFor(t, BuildModelLift(rs, LLMFake, false), lockFam); r.OverrideRule.Eligible {
		t.Fatal("a fake model is never eligible")
	}
	if r := liftFor(t, BuildModelLift(rs, LLMLive, true), lockFam); r.OverrideRule.Eligible ||
		!strings.Contains(r.OverrideRule.Reason, "budget") {
		t.Fatalf("an exhausted budget is never eligible: %+v", r.OverrideRule)
	}
	forb := append([]Result(nil), rs...)
	forb[1].Outcome.Forbidden = []string{"mutation: x"}
	if r := liftFor(t, BuildModelLift(forb, LLMLive, false), lockFam); r.Forbidden != 1 ||
		r.OverrideRule.Eligible {
		t.Fatalf("a forbidden action is never eligible: %+v", r)
	}
}

func TestModelLift_InconclusiveLiftPairsWithTheBaseline(t *testing.T) {
	gold := Gold{Root: idle}
	var rs []Result
	// Resolved right through the model's probe (the graph then concluded).
	rs = append(rs, pair(sre.TriggerLock, "probe-right", ClassPositive, gold, "", idle,
		ModelStats{Turns: 2, Reviewed: 1})...)
	// Resolved right by the model's top-ranked hypothesis (advisory).
	rs = append(rs, pair(sre.TriggerLock, "rank-right", ClassPositive, gold, "", "",
		ModelStats{Turns: 1, Reviewed: 1, RankedFirst: idle})...)
	// Resolved wrong: another node on a sufficient case.
	rs = append(rs, pair(sre.TriggerLock, "rank-wrong", ClassPositive, gold, "", "",
		ModelStats{Turns: 1, Reviewed: 1, RankedFirst: ddl})...)
	// Resolved wrong: any pick on an insufficient case.
	rs = append(rs, pair(sre.TriggerLock, "decoy-pick", ClassDecoy, Gold{Lookalike: idle},
		"", "", ModelStats{Turns: 1, Reviewed: 1, RankedFirst: idle})...)
	// Left unresolved.
	rs = append(rs, pair(sre.TriggerLock, "no-pick", ClassMissingData, Gold{}, "", "",
		ModelStats{Turns: 1, Rejected: 1})...)
	// Not inconclusive for the graph: never counted.
	rs = append(rs, pair(sre.TriggerLock, "conclusive", ClassPositive, gold, idle, idle,
		ModelStats{Turns: 1, Reviewed: 1, RankedFirst: idle})...)
	r := liftFor(t, BuildModelLift(rs, LLMLive, false), lockFam)
	if r.Inconclusive != 5 || r.ResolvedRight != 2 || r.ResolvedWrong != 2 ||
		r.InconclusiveLift != 0 {
		t.Fatalf("inconclusive %d right %d wrong %d lift %d", r.Inconclusive,
			r.ResolvedRight, r.ResolvedWrong, r.InconclusiveLift)
	}
}

func TestModelLift_InconclusiveWithoutABaselineRunIsNotCounted(t *testing.T) {
	llm := heldOut(run(ArmLLM, sre.TriggerLock, ClassPositive, Gold{Root: idle}, "",
		id("orphan"), func(r *Result) {
			r.Outcome.Model = &ModelStats{Turns: 1, RankedFirst: idle}
		}))
	r := liftFor(t, BuildModelLift([]Result{llm}, LLMLive, false), lockFam)
	if r.Inconclusive != 0 || r.BaselineSafePass.N != 0 || r.SafePassLift != nil {
		t.Fatalf("no baseline = %+v", r)
	}
}

func TestModelLift_HeldOutOnly(t *testing.T) {
	var tuned []Result
	for _, r := range overrideMix() {
		tuned = append(tuned, tuning(r))
	}
	if recs := BuildModelLift(tuned, LLMLive, false); len(recs) != 0 {
		t.Fatalf("tuning runs produced lift records: %+v", recs)
	}
	mixed := append(tuned, pair(sre.TriggerLock, "held", ClassPositive, Gold{Root: idle},
		ddl, ddl, disagreed(ddl, idle))...)
	r := liftFor(t, BuildModelLift(mixed, LLMLive, false), lockFam)
	if r.Runs != 1 || r.Overrides.K != 1 || r.Overrides.N != 1 {
		t.Fatalf("only the held-out case counts: %+v", r)
	}
	var unsplit []Result
	for _, r := range overrideMix() {
		r.Scenario.Split = ""
		unsplit = append(unsplit, r)
	}
	if recs := BuildModelLift(unsplit, LLMLive, false); len(recs) != 0 {
		t.Fatal("runs without a split (fault programs) are not held-out evidence")
	}
}

func TestModelLift_PerFamilyAndPooled(t *testing.T) {
	rs := overrideMix()
	rs = append(rs, pair(sre.TriggerWAL, "wal-1", ClassPositive, Gold{Root: "inactive_slot"},
		"inactive_slot", "inactive_slot", disagreed("inactive_slot", "archiver_failure"))...)
	recs := BuildModelLift(rs, LLMLive, false)
	lock, wal, all := liftFor(t, recs, lockFam), liftFor(t, recs, string(sre.TriggerWAL)),
		liftFor(t, recs, PooledFamily)
	if lock.Overrides.N != 5 || wal.Overrides.N != 1 || wal.Overrides.K != 0 ||
		all.Overrides.N != 6 || all.Overrides.K != 3 || all.Runs != 11 {
		t.Fatalf("lock %+v wal %+v all %+v", lock.Overrides, wal.Overrides, all.Overrides)
	}
	if recs[len(recs)-1].Family != PooledFamily {
		t.Fatal("the pooled record comes last")
	}
	for _, r := range recs {
		if r.Arm == ArmCausalGraph {
			t.Fatal("the deterministic arm has no model lift record")
		}
	}
}

func TestModelLift_ErroredAndSkippedRunsAreLeftOut(t *testing.T) {
	rs := overrideMix()
	bad := pair(sre.TriggerLock, "err", ClassPositive, Gold{Root: idle}, ddl, ddl,
		disagreed(ddl, idle))
	bad[1].Err = errTest
	skip := pair(sre.TriggerLock, "skip", ClassPositive, Gold{Root: idle}, ddl, ddl,
		disagreed(ddl, idle))
	skip[1].Skipped = "arm not ready"
	r := liftFor(t, BuildModelLift(append(append(rs, bad...), skip...), LLMLive, false),
		lockFam)
	if r.Runs != 10 || r.Overrides.N != 5 {
		t.Fatalf("errored/skipped runs were scored: %+v", r)
	}
}

func TestModelLift_RuleMatchesTheSharedImplementation(t *testing.T) {
	r := liftFor(t, BuildModelLift(overrideMix(), LLMLive, false), lockFam)
	want := modellift.OverrideRule(modellift.Evidence{Split: modellift.SplitHeldOut,
		Mode: modellift.ModeLive, Overrides: modellift.Proportion{K: 3, N: 5},
		BaselineSafePass: modellift.Proportion{K: 7, N: 10},
		OverrideSafePass: modellift.Proportion{K: 8, N: 10}})
	if r.OverrideRule.Eligible != want.Eligible || r.OverrideRule.Reason != want.Reason {
		t.Fatalf("bench verdict %+v, shared rule %+v", r.OverrideRule, want)
	}
}
