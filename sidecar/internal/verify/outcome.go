package verify

import (
	"fmt"
	"math"
	"time"
)

// Outcome verdicts (Phase 1.3). Only improved earns trust; neutral,
// insufficient evidence and unverifiable are neutral for trust; regressed
// triggers the rollback path. Pending is a prediction not yet judged.
const (
	OutcomePending      = "pending"
	OutcomeImproved     = "improved"
	OutcomeNeutral      = "neutral"
	OutcomeRegressed    = "regressed"
	OutcomeInsufficient = "insufficient_evidence"
	OutcomeUnverifiable = "unverifiable"
)

// Tolerance verdicts: how the observed change compares with the
// predicted one.
const (
	ToleranceMet          = "met"
	TolerancePartial      = "partial"
	ToleranceMissed       = "missed"
	ToleranceNoPrediction = "no_prediction"
	ToleranceUnmeasured   = "unmeasured"
)

// Prediction methods.
const (
	MethodHypoPG = "hypopg"
	MethodModel  = "model"
	MethodRule   = "rule"
	MethodNone   = "none"
)

// Action classes verified against what they targeted.
const (
	ClassIndexCreate = "index_create"
	ClassIndexDrop   = "index_drop"
	ClassGUC         = "guc"
	ClassReloption   = "reloption"
	ClassVacuum      = "vacuum"
	ClassAnalyze     = "analyze"
	ClassQueryHint   = "query_hint"
	ClassRetention   = "retention"
)

// Metrics a prediction can name (config metrics are named by the
// executor: temp_spills, dead_tuples, hot_updates).
const (
	MetricMeanExecTime     = "mean_exec_time"
	MetricDeadTuples       = "dead_tuples"
	MetricModsSinceAnalyze = "n_mod_since_analyze"
	MetricRowsDeleted      = "rows_deleted"
)

// PredictionTolerance is the share of a predicted change the observed one
// must reach to meet it; NoChangeBandPct is how far a metric predicted
// not to move may move and still meet its prediction.
const (
	PredictionTolerance = 0.5
	NoChangeBandPct     = 10.0
)

// Prediction is the structured effect an action is expected to have,
// recorded before it runs: the queries it targets, the metric and its
// expected relative change (negative = decrease), and how it was
// predicted. Method none means no prediction: never credited.
type Prediction struct {
	Class             string   `json:"class"`
	Method            string   `json:"method"`
	Metric            string   `json:"metric,omitempty"`
	TargetQueryIDs    []int64  `json:"target_queryids,omitempty"`
	ExpectedChangePct *float64 `json:"expected_change_pct,omitempty"`
	Baseline          *float64 `json:"baseline,omitempty"`
	Confidence        float64  `json:"confidence,omitempty"`
	Source            string   `json:"source,omitempty"`
	Note              string   `json:"note,omitempty"`
}

// Predicts reports a real prediction: a known method and an expected change.
func (p Prediction) Predicts() bool {
	switch p.Method {
	case MethodHypoPG, MethodModel, MethodRule:
		return p.ExpectedChangePct != nil
	}
	return false
}

// NoPrediction is the explicit "no prediction" record, with why.
func NoPrediction(class, why string) Prediction {
	return Prediction{Class: class, Method: MethodNone, Note: why}
}

// Observed is what the verifier measured for the predicted metric.
type Observed struct {
	Metric    string   `json:"metric,omitempty"`
	Before    float64  `json:"before"`
	After     float64  `json:"after"`
	ChangePct *float64 `json:"change_pct,omitempty"`
}

// Outcome is one action's predicted vs observed verdict: the record the
// trust system reads (sage.action_outcome).
type Outcome struct {
	ActionLogID int64          `json:"action_log_id"`
	DatabaseID  *int64         `json:"database_id,omitempty"`
	Class       string         `json:"class"`
	Verdict     string         `json:"verdict"`
	Tolerance   string         `json:"tolerance"`
	Predicted   Prediction     `json:"predicted"`
	Observed    Observed       `json:"observed"`
	Evidence    map[string]any `json:"evidence"`
	Reason      string         `json:"reason"`
	WindowStart *time.Time     `json:"window_start,omitempty"`
	WindowEnd   *time.Time     `json:"window_end,omitempty"`
	CreatedAt   *time.Time     `json:"created_at,omitempty"`
	DecidedAt   *time.Time     `json:"decided_at,omitempty"`
}

// DecidedVerdict reports one of the five final verdicts.
func DecidedVerdict(v string) bool {
	switch v {
	case OutcomeImproved, OutcomeNeutral, OutcomeRegressed, OutcomeInsufficient,
		OutcomeUnverifiable:
		return true
	}
	return false
}

// ToleranceVerdict compares the observed change with the prediction. A
// change predicted as zero is met inside NoChangeBandPct; otherwise the
// observed change must go the predicted way and reach
// PredictionTolerance of it (met), go that way but less (partial), or it
// missed.
func ToleranceVerdict(p Prediction, verdict string, observedPct *float64) string {
	if !p.Predicts() {
		return ToleranceNoPrediction
	}
	if verdict == OutcomeInsufficient || verdict == OutcomeUnverifiable ||
		observedPct == nil || math.IsNaN(*observedPct) {
		return ToleranceUnmeasured
	}
	expected, observed := *p.ExpectedChangePct, *observedPct
	if expected == 0 {
		if math.Abs(observed) <= NoChangeBandPct {
			return ToleranceMet
		}
		return ToleranceMissed
	}
	toward := observed
	if expected < 0 {
		toward = -observed
	}
	switch {
	case toward >= math.Abs(expected)*(1-PredictionTolerance):
		return ToleranceMet
	case toward > 0:
		return TolerancePartial
	}
	return ToleranceMissed
}

// TargetComparison is one targeted query's comparison.
type TargetComparison struct {
	QueryID int64
	Comparison
}

// DecideTargets judges the queries an action targets. Each target is
// compared on its own with a Bonferroni-corrected significance, and a
// regression of any one is a regression of the action. Otherwise the
// call-weighted pool of the targets measured on both sides decides.
func DecideTargets(
	before, after map[int64]Measurement, ids []int64, th Thresholds,
) (Comparison, []TargetComparison) {
	th = th.normalized()
	strict := th
	if len(ids) > 1 {
		strict.Alpha = th.Alpha / float64(len(ids))
	}
	per := make([]TargetComparison, 0, len(ids))
	var pooledBefore, pooledAfter []Measurement
	regressed := -1
	for _, id := range ids {
		b, a := before[id], after[id]
		tc := TargetComparison{QueryID: id, Comparison: Compare(b, a, strict)}
		per = append(per, tc)
		if tc.Verdict == OutcomeRegressed && regressed < 0 {
			regressed = len(per) - 1
		}
		if b.Samples > 0 && a.Samples > 0 {
			pooledBefore, pooledAfter = append(pooledBefore, b), append(pooledAfter, a)
		}
	}
	pooled := Compare(Pool(pooledBefore), Pool(pooledAfter), th)
	if regressed >= 0 {
		pooled.Verdict = OutcomeRegressed
		pooled.Reason = fmt.Sprintf("targeted query %d regressed: %s", per[regressed].QueryID,
			per[regressed].Reason)
	}
	return pooled, per
}

// TargetsEvidence is the per-target comparisons as JSON-safe data.
func TargetsEvidence(per []TargetComparison) []map[string]any {
	out := make([]map[string]any, 0, len(per))
	for _, tc := range per {
		e := tc.Evidence()
		e["queryid"] = tc.QueryID
		out = append(out, e)
	}
	return out
}
