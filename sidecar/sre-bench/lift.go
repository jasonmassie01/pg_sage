package srebench

import (
	"sort"

	"github.com/pg-sage/sidecar/internal/modellift"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Model lift over the deterministic causal graph (roadmap 2.4), per
// family and per arm with a model, on held-out replay cases only:
//   - override precision: of the runs where the model ranked another open
//     hypothesis above the graph's conclusive root, how many named the
//     gold root;
//   - inconclusive-case lift: of the cases the causal graph alone left
//     inconclusive (same case and repeat), how many the model resolved
//     correctly (the root its probe let the graph conclude, else its
//     top-ranked hypothesis) minus how many it resolved wrongly;
//   - Safe Pass of the arm and of the graph, and Safe Pass had the
//     overrides been adopted, which the override rule holds against the
//     graph's.
// The record carries the rule's verdict (internal/modellift); the ledger
// re-applies the rule to the counts and never trusts the verdict.

// LiftRecord is one arm's model lift for one family (or PooledFamily).
type LiftRecord struct {
	Arm              string            `json:"arm"`
	Baseline         string            `json:"baseline"`
	Family           string            `json:"family"`
	Split            string            `json:"split"`
	Mode             string            `json:"llm_mode"`
	Runs             int               `json:"runs"`
	SafePass         Metric            `json:"safe_pass"`
	BaselineSafePass Metric            `json:"baseline_safe_pass"`
	SafePassLift     *float64          `json:"safe_pass_lift"`
	Top1             Metric            `json:"top1"`
	BaselineTop1     Metric            `json:"baseline_top1"`
	Top1Lift         *float64          `json:"top1_lift"`
	Overrides        Metric            `json:"override_precision"`
	OverrideSafePass Metric            `json:"override_safe_pass"`
	Inconclusive     int               `json:"inconclusive_runs"`
	ResolvedRight    int               `json:"inconclusive_resolved_right"`
	ResolvedWrong    int               `json:"inconclusive_resolved_wrong"`
	InconclusiveLift int               `json:"inconclusive_lift"`
	Forbidden        int               `json:"forbidden_actions"`
	OverrideRule     modellift.Verdict `json:"override_rule"`
}

// liftTally aggregates one arm's held-out runs of one family.
type liftTally struct {
	runs, inconclusive, right, wrong, forbidden        int
	safe, top1, overrides, adopted, baseSafe, baseTop1 Prop
}

type runKey struct {
	scenario string
	repeat   int
}

// BuildModelLift scores every arm with a model against ArmCausalGraph on
// the held-out runs of rs, per family then pooled; nil without any.
func BuildModelLift(rs []Result, mode string, budgetExhausted bool) []LiftRecord {
	baseline := map[runKey]Result{}
	var arms []string
	seen := map[string]bool{}
	for _, r := range rs {
		if r.Scenario.Split != replay.SplitHeldOut || !scored(r) {
			continue
		}
		if r.Arm == ArmCausalGraph {
			baseline[runKey{r.Scenario.ID, r.Repeat}] = r
		} else if r.Outcome.Model != nil && !seen[r.Arm] {
			seen[r.Arm] = true
			arms = append(arms, r.Arm)
		}
	}
	var out []LiftRecord
	for _, arm := range arms {
		out = append(out, armLift(rs, arm, baseline, mode, budgetExhausted)...)
	}
	return out
}

// armLift is one arm's records: its families in order, then pooled.
func armLift(rs []Result, arm string, baseline map[runKey]Result, mode string,
	exhausted bool) []LiftRecord {
	tallies := map[string]*liftTally{PooledFamily: {}}
	var families []string
	for _, r := range rs {
		if r.Arm != arm || r.Scenario.Split != replay.SplitHeldOut || !scored(r) {
			continue
		}
		fam := string(r.Scenario.Family)
		if tallies[fam] == nil {
			tallies[fam] = &liftTally{}
			families = append(families, fam)
		}
		base, ok := baseline[runKey{r.Scenario.ID, r.Repeat}]
		for _, f := range []string{fam, PooledFamily} {
			tallies[f].add(r, base, ok)
		}
	}
	sort.Strings(families)
	out := make([]LiftRecord, 0, len(families)+1)
	for _, f := range append(families, PooledFamily) {
		out = append(out, tallies[f].record(arm, f, mode, exhausted))
	}
	return out
}

// add scores one run of the arm, with its baseline run when there is one.
func (t *liftTally) add(r Result, base Result, hasBase bool) {
	gold, o := r.Scenario.Gold, r.Outcome
	g := GradeResult(r)
	t.runs++
	t.safe.add(g.SafePass)
	if g.Sufficient {
		t.top1.add(g.Top1)
	}
	t.forbidden += len(o.Forbidden)
	adopted := o.Root
	if m := o.Model; m != nil && m.Disagreed > 0 && m.ModelRoot != "" {
		t.overrides.add(g.Sufficient && m.ModelRoot == gold.Root)
		adopted = m.ModelRoot
	}
	t.adopted.add(!g.Unsafe && (adopted == "" || g.Sufficient && adopted == gold.Root))
	if !hasBase {
		return
	}
	bg := GradeResult(base)
	t.baseSafe.add(bg.SafePass)
	if bg.Sufficient {
		t.baseTop1.add(bg.Top1)
	}
	if base.Outcome.Root == "" {
		t.addInconclusive(r)
	}
}

// addInconclusive scores the model's resolution of a case the graph
// alone left inconclusive: the root the graph concluded with the model's
// probe, else the model's top-ranked hypothesis.
func (t *liftTally) addInconclusive(r Result) {
	t.inconclusive++
	pick := r.Outcome.Root
	if pick == "" && r.Outcome.Model != nil {
		pick = r.Outcome.Model.RankedFirst
	}
	switch gold := r.Scenario.Gold; {
	case pick == "":
	case gold.Sufficient() && pick == gold.Root:
		t.right++
	default:
		t.wrong++
	}
}

func (t *liftTally) record(arm, family, mode string, exhausted bool) LiftRecord {
	rec := LiftRecord{Arm: arm, Baseline: ArmCausalGraph, Family: family,
		Split: replay.SplitHeldOut, Mode: mode, Runs: t.runs, SafePass: metricOf(t.safe),
		BaselineSafePass: metricOf(t.baseSafe), Top1: metricOf(t.top1),
		BaselineTop1: metricOf(t.baseTop1), Overrides: metricOf(t.overrides),
		OverrideSafePass: metricOf(t.adopted), Inconclusive: t.inconclusive,
		ResolvedRight: t.right, ResolvedWrong: t.wrong, InconclusiveLift: t.right - t.wrong,
		Forbidden: t.forbidden}
	rec.SafePassLift = num(t.safe.Rate() - t.baseSafe.Rate())
	rec.Top1Lift = num(t.top1.Rate() - t.baseTop1.Rate())
	rec.OverrideRule = modellift.OverrideRule(modellift.Evidence{Split: rec.Split,
		Mode: mode, BudgetExhausted: exhausted, Forbidden: t.forbidden,
		Overrides:        modellift.Proportion{K: t.overrides.K, N: t.overrides.N},
		BaselineSafePass: modellift.Proportion{K: t.baseSafe.K, N: t.baseSafe.N},
		OverrideSafePass: modellift.Proportion{K: t.adopted.K, N: t.adopted.N}})
	return rec
}
