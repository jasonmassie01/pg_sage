package srebench

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Owner decision (2026-10-04, PR #116): the nightly and tag live-model
// run also measures the tool-calling investigator, inside the existing
// caps. The caps cover one model arm's replay of the corpus, not both,
// so each run measures one model arm, named by SAGE_BENCH_LIVE_MODEL_ARM
// (the workflow alternates it by night; a v* tag measures the review
// arm, whose lift the release's root authority reads).

func TestLiveModelArmFromEnv(t *testing.T) {
	llm := LLMConfig{Mode: LLMLive, URL: "https://x.example/v1", Model: "m"}
	for v, want := range map[string]string{"": ArmLLM, ArmLLM: ArmLLM,
		" " + ArmInvestigator + " ": ArmInvestigator} {
		arm, err := LiveModelArmFromEnv(env(map[string]string{EnvLiveModelArm: v}), llm)
		if err != nil || arm.Name() != want {
			t.Errorf("%q: arm %v err %v, want %s", v, arm, err, want)
			continue
		}
		if ok, why := arm.Ready(); !ok {
			t.Errorf("%q: arm not ready: %s", v, why)
		}
	}
	for _, v := range []string{ArmCausalGraph, ArmRulesOnly, "both", "investigator"} {
		if _, err := LiveModelArmFromEnv(env(map[string]string{EnvLiveModelArm: v}),
			llm); err == nil || !strings.Contains(err.Error(), EnvLiveModelArm) {
			t.Errorf("%q: err %v, want a refusal naming %s", v, err, EnvLiveModelArm)
		}
	}
}

func TestBuildLiveReport_MeasuresTheInvestigatorArm(t *testing.T) {
	cfg := LLMConfig{Mode: LLMLive, Model: "gpt-4o-mini",
		budget: NewBudget(caps(), time.Now)}
	var rs []Result
	for _, r := range overrideMix() {
		if r.Arm == ArmLLM {
			r.Arm = ArmInvestigator
		}
		rs = append(rs, r)
	}
	cases := []replay.Case{{ID: "c1", Family: "lock_blocking", Class: replay.ClassPositive}}
	r := BuildLiveReport(rs, cases, LiveMeta{LLM: cfg, Arm: ArmInvestigator,
		GeneratedAt: time.Now().UTC(), ServerVersion: "PostgreSQL 17"})
	if r.Replay == nil || strings.Join(r.Replay.Arms, ",") !=
		ArmCausalGraph+","+ArmInvestigator || strings.Join(r.Arms, ",") !=
		ArmCausalGraph+","+ArmInvestigator {
		t.Fatalf("live report arms = %v / %v", r.Arms, r.Replay)
	}
	found := false
	for _, l := range r.ModelLift {
		if l.Arm == ArmLLM {
			t.Fatalf("an investigator night reported a review-arm lift: %+v", l)
		}
		found = found || l.Arm == ArmInvestigator
	}
	if !found {
		t.Fatalf("no investigator lift in %+v", r.ModelLift)
	}
	if fails, _ := CheckLiveReport(r); len(fails) != 0 {
		t.Fatalf("a clean investigator night failed: %v", fails)
	}
	if def := liveReport(t, NewBudget(caps(), time.Now)); strings.Join(def.Arms, ",") !=
		ArmCausalGraph+","+ArmLLM {
		t.Fatalf("the default live report arms = %v", def.Arms)
	}
}
