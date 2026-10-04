package verify

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Statistics verification (dogfood round 2). CREATE STATISTICS exists to
// fix the planner's row estimates, so it is judged on them: the q-error
// (max(estimate, actual) / min(estimate, actual), per loop) of sampled
// plans with actual rows of the queries it targets, before vs after, in
// doublings. The targets' call-weighted time is the harm check, and the
// only evidence when no plan with actual rows is ever sampled.

// DefaultMinEstimatePlans is the fewest sampled plans with actual rows a
// window needs for its estimate error to count.
const DefaultMinEstimatePlans = 3

// estimateDoubling is the change, in doublings of the q-error (log2), that
// counts: halved error is improved, doubled error is regressed.
const estimateDoubling = 1.0

// PlanQError is the worst row-estimate error of a plan with actual rows
// (EXPLAIN ANALYZE or auto_explain JSON): the largest per-node q-error,
// with both counts floored at one row. Nodes below a Limit (stopped early
// by design) and nodes never executed are skipped. ok is false when the
// plan has no node with actual rows or cannot be read.
func PlanQError(plan []byte) (float64, bool) {
	var doc any
	if len(plan) == 0 || json.Unmarshal(plan, &doc) != nil {
		return 0, false
	}
	var worst float64
	found := false
	for _, root := range planRoots(doc) {
		walkQError(root, false, &worst, &found)
	}
	return worst, found
}

// planRoots finds the plan nodes of an EXPLAIN JSON array, an auto_explain
// object ({"Plan": ...}) or a bare node.
func planRoots(doc any) []map[string]any {
	var out []map[string]any
	switch v := doc.(type) {
	case []any:
		for _, item := range v {
			out = append(out, planRoots(item)...)
		}
	case map[string]any:
		if node, ok := v["Plan"].(map[string]any); ok {
			return []map[string]any{node}
		}
		if _, ok := v["Node Type"]; ok {
			return []map[string]any{v}
		}
	}
	return out
}

func walkQError(node map[string]any, underLimit bool, worst *float64, found *bool) {
	if !underLimit {
		if q, ok := nodeQError(node); ok {
			*found = true
			*worst = math.Max(*worst, q)
		}
	}
	limit := underLimit || node["Node Type"] == "Limit"
	children, _ := node["Plans"].([]any)
	for _, c := range children {
		if child, ok := c.(map[string]any); ok {
			walkQError(child, limit, worst, found)
		}
	}
}

func nodeQError(node map[string]any) (float64, bool) {
	est, okEst := node["Plan Rows"].(float64)
	act, okAct := node["Actual Rows"].(float64)
	loops, okLoops := node["Actual Loops"].(float64)
	if !okEst || !okAct || !okLoops || loops <= 0 {
		return 0, false
	}
	est, act = math.Max(est, 1), math.Max(act, 1)
	return math.Max(est/act, act/est), true
}

// EstimateSample is one window's row-estimate error: how many sampled
// plans with actual rows it had and their median worst-node q-error.
type EstimateSample struct {
	Plans  int     `json:"plans"`
	QError float64 `json:"q_error"`
}

// SummarizeEstimates is the median of per-plan q-errors (the geometric
// mean of the two middle ones for an even count: errors are ratios).
// Values that are not finite q-errors (< 1) are dropped.
func SummarizeEstimates(qerrors []float64) EstimateSample {
	valid := make([]float64, 0, len(qerrors))
	for _, q := range qerrors {
		if q >= 1 && !math.IsInf(q, 0) {
			valid = append(valid, q)
		}
	}
	if len(valid) == 0 {
		return EstimateSample{}
	}
	sort.Float64s(valid)
	n := len(valid)
	median := valid[n/2]
	if n%2 == 0 {
		median = math.Sqrt(valid[n/2-1] * valid[n/2])
	}
	return EstimateSample{Plans: n, QError: median}
}

// EstimateJudgement is the row-estimate verdict. Terminal marks an
// insufficient verdict time cannot change: no plan with actual rows was
// sampled before the action, so there is no baseline to compare with.
type EstimateJudgement struct {
	Verdict   string
	Reason    string
	ChangePct *float64
	Before    float64
	After     float64
	Terminal  bool
}

// JudgeEstimates compares the targets' estimate error before and after:
// improved when it at least halved, regressed when it at least doubled,
// neutral in between; insufficient with fewer than minPlans (0: the
// default) sampled plans on either side.
func JudgeEstimates(before, after EstimateSample, minPlans int) EstimateJudgement {
	if minPlans <= 0 {
		minPlans = DefaultMinEstimatePlans
	}
	j := EstimateJudgement{Before: before.QError, After: after.QError}
	switch {
	case before.Plans == 0:
		j.Verdict, j.Terminal = OutcomeInsufficient, true
		j.Reason = "no sampled plan with actual rows before the action: no estimate baseline"
		return j
	case before.Plans < minPlans || after.Plans < minPlans:
		j.Verdict = OutcomeInsufficient
		j.Reason = fmt.Sprintf("%d plans with actual rows before and %d after; %d each "+
			"needed", before.Plans, after.Plans, minPlans)
		return j
	}
	change := (after.QError - before.QError) * 100 / before.QError
	j.ChangePct = &change
	delta := math.Log2(after.QError) - math.Log2(before.QError)
	switch {
	case delta >= estimateDoubling:
		j.Verdict = OutcomeRegressed
	case delta <= -estimateDoubling:
		j.Verdict = OutcomeImproved
	default:
		j.Verdict = OutcomeNeutral
	}
	j.Reason = fmt.Sprintf("row-estimate error %s: q %.3g -> %.3g (%d -> %d plans)",
		estimateWord(j.Verdict), before.QError, after.QError, before.Plans, after.Plans)
	return j
}

func estimateWord(verdict string) string {
	switch verdict {
	case OutcomeImproved:
		return "at least halved"
	case OutcomeRegressed:
		return "at least doubled"
	}
	return "held within 2x"
}

// DecideStatistics combines the targets' call-weighted time comparison
// with the row-estimate judgement; accruing reports a verdict more time
// can still change. A regression of either is a regression; better
// estimates count once latency is measured not to have regressed; without
// estimate evidence the latency decides.
func DecideStatistics(time *Comparison, est EstimateJudgement) (string, string, bool) {
	switch {
	case time != nil && time.Verdict == OutcomeRegressed:
		return OutcomeRegressed, "targeted queries regressed: " + time.Reason, false
	case est.Verdict == OutcomeRegressed:
		return OutcomeRegressed, est.Reason, false
	case time == nil:
		return OutcomeUnverifiable, "no targeted query was measured: not credited", false
	case time.Verdict == OutcomeInsufficient:
		return OutcomeInsufficient, est.Reason + "; latency not yet measurable: " +
			time.Reason, true
	case est.Verdict == OutcomeImproved || time.Verdict == OutcomeImproved:
		return OutcomeImproved, est.Reason + "; latency " + time.Verdict + ": " +
			time.Reason, false
	case est.Verdict == OutcomeInsufficient && !est.Terminal:
		return OutcomeInsufficient, est.Reason + "; latency held", true
	}
	return OutcomeNeutral, est.Reason + "; latency held: " + time.Reason, false
}
