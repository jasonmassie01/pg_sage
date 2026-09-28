package causal

import (
	"fmt"
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Plan regression family: query_store.plan_hash flips joined with the
// windowed latency change from the plan_regressions probe.

const (
	// RegressionRatio is the after/before latency ratio that counts as a
	// regression.
	RegressionRatio = 1.5
	// MaxPlanDiagnoses bounds the regressed queries diagnosed at once.
	MaxPlanDiagnoses = 5
	strongRatio      = 3
	strongCalls      = 10
)

// DiagnosePlan returns one diagnosis per regressed query (largest ratio
// first, at most MaxPlanDiagnoses). Queries whose latency did not
// regress are not incidents and are not reported. An unavailable probe
// yields one inconclusive diagnosis naming the missing evidence.
func DiagnosePlan(obs []Observation) []Diagnosis {
	o, ok := find(obs, probes.PlanRegressions)
	shifts, err := probes.PlanShifts(o.Result)
	if !ok || err != nil {
		d := Diagnosis{Family: FamilyPlanRegression, GraphVersion: GraphVersion,
			Missing: missingFor(obs, probes.PlanRegressions),
			Reason:  "plan regression evidence unavailable"}
		return []Diagnosis{d}
	}
	var out []Diagnosis
	for _, s := range shifts {
		ratio := s.Ratio()
		if math.IsNaN(ratio) || ratio < RegressionRatio ||
			s.BeforeCalls < 1 || s.AfterCalls < 1 {
			continue
		}
		out = append(out, diagnoseShift(s, ratio, o.EvidenceID))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ratio > out[j].Ratio })
	if len(out) > MaxPlanDiagnoses {
		out = out[:MaxPlanDiagnoses]
	}
	return out
}

func diagnoseShift(s probes.PlanShift, ratio float64, ev string) Diagnosis {
	subject := fmt.Sprintf("queryid %d", s.QueryID)
	flip := newHypothesis(PlanFlipRegression, subject)
	same := newHypothesis(SamePlanLatencyRegression, subject)
	latency := fmt.Sprintf("mean latency rose from %s ms (%d calls) to %s ms "+
		"(%d calls), ratio %s", probes.FormatValue(s.BeforeMeanMS), s.BeforeCalls,
		probes.FormatValue(s.AfterMeanMS), s.AfterCalls, probes.FormatValue(ratio))
	winner := &same
	if s.Flipped {
		winner = &flip
		flip.add(0.4, ev, fmt.Sprintf("plan_hash changed from %s to %s at %s",
			s.PreviousHash, s.CurrentHash, probes.FormatValue(s.FlippedAt)))
		flip.add(0.3, ev, latency)
		same.contradict(ev, fmt.Sprintf("the plan changed at the regression point "+
			"(%s to %s)", s.PreviousHash, s.CurrentHash))
	} else {
		same.add(0.5, ev, fmt.Sprintf("plan_hash %s is unchanged; %s", s.CurrentHash,
			latency))
		flip.contradict(ev, fmt.Sprintf("plan_hash unchanged (%s) across the window",
			s.CurrentHash))
	}
	if ratio >= strongRatio {
		winner.add(0.1, ev, "the latency ratio is at least 3")
	}
	if s.BeforeCalls >= strongCalls && s.AfterCalls >= strongCalls {
		winner.add(0.1, ev, "at least 10 calls on each side of the change")
	}
	d := rank(FamilyPlanRegression, []Hypothesis{flip, same})
	d.Subject, d.Ratio = subject, ratio
	return d
}
