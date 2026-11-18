package verify

import (
	"fmt"
	"time"
)

// WhatIfEvidence is a HypoPG what-if's result for one index, as the
// optimizer or the tuning agent recorded it: the verdict ("verified",
// "unverified", "rejected"), the predicted planned-cost change in percent,
// the statements measured and those left unplanned, whether generic plans
// (PG16+) were used, and why HypoPG cannot model the index ("" when it
// can).
type WhatIfEvidence struct {
	Verdict     string
	Reason      string
	ChangePct   *float64
	Measured    int
	Unplanned   int
	GenericPlan bool
	CannotModel string
}

// HintComparison is a query hint's plan comparison (tuning-05): the
// verdict and the hinted plan's cost change in percent.
type HintComparison struct {
	Verdict      string
	Reason       string
	CostDeltaPct *float64
	GenericPlan  bool
}

// Rehearsal is a clone rehearsal's result (lab-04a fills it): passed, or
// inconclusive (the rehearsal could not judge, which wins over passed),
// else failed; the change of blocks per call it measured, the receipt and
// the clone provider.
type Rehearsal struct {
	Passed           bool
	Inconclusive     bool
	Reason           string
	BlocksPerCallPct *float64
	ReceiptID        int64
	Provider         string
}

// FromWhatIf is the tier-1 record of an index create's HypoPG what-if. A
// method that cannot model the index type gives no pass and no number.
func FromWhatIf(e WhatIfEvidence, at time.Time) LadderRecord {
	return whatIfRecord(MethodLadderWhatIf, e, at)
}

// FromDropMeasurement is the tier-1 record of a drop's HypoPG hide what-if
// (Optimizer.MeasureDrop).
func FromDropMeasurement(e WhatIfEvidence, at time.Time) LadderRecord {
	return whatIfRecord(MethodLadderHide, e, at)
}

func whatIfRecord(method string, e WhatIfEvidence, at time.Time) LadderRecord {
	if e.CannotModel != "" {
		return skippedRecord(TierPlanOnly, method, e.CannotModel, at)
	}
	r := newRecord(TierPlanOnly, method, LadderPassed, at)
	r.Predicted, r.Metric = e.ChangePct, LadderMetricPlanCost
	if e.Measured > 0 {
		r.Evidence["measured"] = e.Measured
	}
	if e.Unplanned > 0 {
		r.Evidence["unplanned"] = e.Unplanned
	}
	if e.GenericPlan {
		r.Evidence["generic_plan"] = true
	}
	if e.Reason != "" {
		r.Evidence["reason"] = e.Reason
	}
	switch e.Verdict {
	case "verified":
		return r
	case "rejected":
		r.Result = LadderFailed
		return r
	}
	why := e.Reason
	if why == "" {
		why = fmt.Sprintf("the what-if verdict is %q, not verified", e.Verdict)
	}
	return inconclusive(r, why)
}

// FromHintEvidence is the tier-1 record of a query hint's plan comparison.
func FromHintEvidence(h HintComparison, at time.Time) LadderRecord {
	r := newRecord(TierPlanOnly, MethodLadderHint, LadderPassed, at)
	r.Predicted, r.Metric = h.CostDeltaPct, LadderMetricPlanCost
	r.Evidence["verdict"] = h.Verdict
	if h.GenericPlan {
		r.Evidence["generic_plan"] = true
	}
	if h.Reason != "" {
		r.Evidence["reason"] = h.Reason
	}
	switch h.Verdict {
	case "changed":
		return r
	case "no_effect", "cost_regression":
		r.Result = LadderFailed
		return r
	}
	why := h.Reason
	if why == "" {
		why = fmt.Sprintf("the plan comparison is %q", h.Verdict)
	}
	return inconclusive(r, why)
}

// FromRehearsal is the tier-2 record of a clone rehearsal.
func FromRehearsal(reh Rehearsal, at time.Time) LadderRecord {
	r := newRecord(TierClone, MethodLadderRehearsal, LadderFailed, at)
	if reh.BlocksPerCallPct != nil {
		r.Predicted, r.Metric = reh.BlocksPerCallPct, LadderMetricBlocksPerCall
	}
	if reh.ReceiptID > 0 {
		r.Evidence["receipt_id"] = reh.ReceiptID
	}
	if reh.Provider != "" {
		r.Evidence["provider"] = reh.Provider
	}
	if reh.Reason != "" {
		r.Evidence["reason"] = reh.Reason
	}
	// Inconclusive first: a result that says both passed and could not
	// judge is contradictory and fails closed.
	switch {
	case reh.Inconclusive:
		return inconclusive(r, reh.Reason)
	case reh.Passed:
		r.Result = LadderPassed
	}
	return r
}

// FromVerdict is the tier-3 record of the production verdict: a kept
// change (improved or neutral) passed, a regression failed, anything else
// is inconclusive.
func FromVerdict(verdict string, changePct *float64, actionID int64,
	at time.Time) LadderRecord {
	r := newRecord(TierCanary, MethodLadderVerdict, LadderPassed, at)
	r.Predicted = changePct
	r.Evidence["verdict"] = verdict
	if actionID > 0 {
		r.Evidence["action_id"] = actionID
	}
	switch verdict {
	case OutcomeImproved, OutcomeNeutral:
		return r
	case OutcomeRegressed:
		r.Result = LadderFailed
		return r
	}
	return inconclusive(r, fmt.Sprintf("the verdict is %q", verdict))
}
