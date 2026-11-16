package rehearsal

import (
	"fmt"
	"strconv"
)

// LatencyNoiseFloorMS is the smallest latency rise, in milliseconds, that
// counts as a regression: one clone execution of a sub-millisecond
// statement moves by more than any percentage threshold on noise alone.
// Plan costs are deterministic and have no floor.
const LatencyNoiseFloorMS = 1.0

// Why an affected statement gives no evidence (QueryMeasurement.Unmeasured).
const (
	UnmeasuredTimedOutBefore = "timed out on the clone before the expand steps"
	UnmeasuredTimedOutAfter  = "timed out on the clone after the expand steps"
	UnmeasuredNotOne         = "skipped: the text holds more than one statement"
)

// judge turns a measurement into a verdict. A step over its budget
// parks first: that evidence stands without a workload. A measured
// statement that regressed parks next. Without a measured statement the
// result is inconclusive and never promotes, nor when a statement that
// ran before the expand steps timed out after them.
func (o *Orchestrator) judge(measurement Measurement) Result {
	result := Result{Verdict: VerdictPromoteExpand, Reason: ReasonRehearsalPassed,
		Measurement: measurement}
	if detail, over := o.overBudget(measurement); over {
		result.Verdict, result.Reason, result.Detail = VerdictPark, ReasonStepBudget, detail
		return result
	}
	if detail, ok := regressed(measurement, o.options.RegressionPct, o.planFlipPct()); ok {
		result.Verdict, result.Reason, result.Detail = VerdictPark, ReasonPlanRegression, detail
		return result
	}
	if detail, ok := inconclusive(measurement); ok {
		result.Verdict, result.Reason = VerdictRecommendOnly, ReasonInconclusiveWorkload
		result.Detail = detail
	}
	return result
}

// judged is what a statement is compared on: latency when it executed on
// both sides, plan cost when it was planned on both sides.
type judged struct {
	before, after, floor float64
	unit                 string
}

// metric is the statement's judged pair; false when it gives no evidence.
func (q QueryMeasurement) metric() (judged, bool) {
	switch {
	case q.Unmeasured != "":
		return judged{}, false
	case q.BeforeLatencyMS > 0 && q.AfterLatencyMS > 0:
		return judged{q.BeforeLatencyMS, q.AfterLatencyMS, LatencyNoiseFloorMS, "ms"}, true
	case q.BeforeLatencyMS == 0 && q.BeforeCost > 0 && q.AfterCost > 0:
		return judged{q.BeforeCost, q.AfterCost, 0, "cost"}, true
	}
	return judged{}, false
}

// regressed names the first statement slower by more than threshold
// percent, or by more than flipPct percent when its plan hash changed,
// and by more than the metric's noise floor.
func regressed(measurement Measurement, threshold, flipPct float64) (string, bool) {
	for _, query := range measurement.AffectedQueries {
		m, ok := query.metric()
		if !ok {
			continue
		}
		change := (m.after - m.before) * 100 / m.before
		flipped := query.AfterPlanHash != query.BeforePlanHash
		if m.after-m.before <= m.floor ||
			!(change > threshold || (flipped && change > flipPct)) {
			continue
		}
		detail := fmt.Sprintf("queryid %d: %s %s to %s (%+.1f%%)", query.QueryID,
			m.unit, number(m.before), number(m.after), change)
		if flipped {
			detail += ", plan changed"
		}
		return detail, true
	}
	return "", false
}

// inconclusive says why the measurement cannot promote, if it cannot.
func inconclusive(measurement Measurement) (string, bool) {
	measured, firstGap := 0, ""
	for _, query := range measurement.AffectedQueries {
		if query.Unmeasured == UnmeasuredTimedOutAfter {
			return fmt.Sprintf("queryid %d %s, not before them", query.QueryID,
				UnmeasuredTimedOutAfter), true
		}
		if _, ok := query.metric(); ok {
			measured++
			continue
		}
		if firstGap == "" {
			firstGap = fmt.Sprintf("queryid %d: %s", query.QueryID, gapReason(query))
		}
	}
	switch {
	case measured > 0:
		return "", false
	case len(measurement.AffectedQueries) == 0 && measurement.WorkloadNote != "":
		return measurement.WorkloadNote, true
	case len(measurement.AffectedQueries) == 0:
		return "", true
	}
	return "no affected statement was measured on the clone (" + firstGap + ")", true
}

func gapReason(query QueryMeasurement) string {
	switch {
	case query.Unmeasured != "":
		return query.Unmeasured
	case query.NotExecuted != "":
		return query.NotExecuted
	}
	return "no baseline"
}

func number(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
