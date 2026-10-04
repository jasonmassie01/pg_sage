package srebench

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// The nightly live-model arm (roadmap 2.4): the replay corpus through
// the causal graph and the LLM-on arm against a live model, within the
// run's hard caps, reported as a PGIncidentBench report of schema
// revision 2 with no fault-program cells (so it never stands in for the
// release bench in promotions) and the held-out model lift, which the
// ledger reads for each family's model-root authority.

// EnvLiveArm must be "1" for TestLiveModelArm to run.
const EnvLiveArm = "SAGE_BENCH_LIVE_ARM"

// LiveArmRequested reports whether EnvLiveArm asks for the live arm.
func LiveArmRequested(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(EnvLiveArm)) == "1"
}

// LiveMeta describes a live-arm run.
type LiveMeta struct {
	LLM                         LLMConfig
	GeneratedAt                 time.Time
	ServerVersion               string
	PgSageVersion, PgSageCommit string
}

// BuildLiveReport scores a live-arm replay as a report: replay section,
// model lift and the budget, stamped with the pg_sage build.
func BuildLiveReport(rs []Result, cases []replay.Case, meta LiveMeta) Report {
	arms := []string{ArmCausalGraph, ArmLLM}
	r := BuildReport(nil, ReportMeta{Arms: arms, Gated: arms, Repeats: 1,
		ServerVersion: meta.ServerVersion, GeneratedAt: meta.GeneratedAt, LLM: meta.LLM,
		PgSageVersion: meta.PgSageVersion, PgSageCommit: meta.PgSageCommit})
	rep := BuildReplayReport(rs, cases, ReplayMeta{Arms: arms, Gated: arms, LLM: meta.LLM,
		GeneratedAt: meta.GeneratedAt, ServerVersion: meta.ServerVersion})
	r.AttachReplay(&rep)
	return r
}

// safetyGates fail a live run; the quality gates are measured, not
// reasons to fail it.
var safetyGates = map[string]bool{GateForbidden: true, GateAdversarial: true,
	GateClaimRefs: true, GateLLMRoot: true}

// CheckLiveReport lists what fails a live run (an exhausted budget, a
// missing replay, a failed safety gate) and notes the failed quality
// gates.
func CheckLiveReport(r Report) (failures, notes []string) {
	if b := r.LLMBudget; b != nil && b.Exhausted {
		failures = append(failures, "live model budget exhausted (fail closed): "+b.Reason)
	}
	if r.Replay == nil || len(r.ModelLift) == 0 {
		return append(failures, "the live run measured no held-out model lift"), notes
	}
	for _, g := range FailedGates(r.Replay.Gates) {
		text := fmt.Sprintf("gate %s (%s, %s) failed: observed %s, threshold %s", g.ID,
			g.Family, g.Arm, g.Observed, g.Threshold)
		if safetyGates[g.ID] {
			failures = append(failures, text)
		} else {
			notes = append(notes, "measured, not failing the run: "+text)
		}
	}
	return failures, notes
}
