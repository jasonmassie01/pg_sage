package receipt

import (
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Receipt reasons (Receipt.Reason).
const (
	// ReasonNoChange: the watched statements kept their latency (clean).
	ReasonNoChange = "no_change"
	// ReasonLowTraffic: too few calls or sampling intervals to compare,
	// and no harm signal (clean_low_traffic).
	ReasonLowTraffic = "low_traffic"
	// ReasonLatency: a watched statement got slower (regressed).
	ReasonLatency = "latency"
	// ReasonLock: a lock incident or lock_chain finding on a touched
	// relation around landing (regressed).
	ReasonLock = "lock"
	// ReasonConfounded: a statistics reset, restart or failover inside a
	// window (unverifiable).
	ReasonConfounded = "confounded"
)

// RegressPct is the practical regression bar of a watched statement, the
// same 15% verify uses for pg_sage's own actions.
const RegressPct = verify.DefaultRegressPct

// maxWindowHours is change_review.receipt_window_hours' upper bound.
const maxWindowHours = 168

// Windows are a landed review's comparison windows: before is
// [landed_lower - W, landed_lower], after is [landed_at, landed_at + W].
type Windows struct {
	BeforeFrom time.Time
	BeforeTo   time.Time
	AfterFrom  time.Time
	AfterTo    time.Time
}

// WindowsOf is r's windows for a W of hours. r must have landed.
func WindowsOf(r changereview.Review, hours int) (Windows, error) {
	if r.LandingState != changereview.LandingLanded || r.LandedLower == nil ||
		r.LandedAt == nil {
		return Windows{}, fmt.Errorf("%w: review %s has not landed (%s)",
			changereview.ErrInvalid, r.ID, r.LandingState)
	}
	if hours < 1 || hours > maxWindowHours {
		return Windows{}, fmt.Errorf("%w: receipt window of %d hours is not 1-%d",
			changereview.ErrInvalid, hours, maxWindowHours)
	}
	w := time.Duration(hours) * time.Hour
	lower, at := r.LandedLower.UTC(), r.LandedAt.UTC()
	return Windows{BeforeFrom: lower.Add(-w), BeforeTo: lower, AfterFrom: at,
		AfterTo: at.Add(w)}, nil
}

// Observations are what the receipt rests on: the watched statements'
// measurements in each window, confounding change events, lock incidents
// (ids) and lock_chain findings (ids) on touched relations around landing,
// and the born queryids.
type Observations struct {
	Before       map[int64]verify.Measurement
	After        map[int64]verify.Measurement
	Confounders  []string
	Incidents    []string
	LockFindings []int64
	Born         []changereview.BornQuery
}

// Decide is the receipt of r over w. Harm ranks first: lock harm on a
// touched relation regresses it; then a confounder makes it unverifiable;
// then the watched statements decide (verify.DecideTargets, Welch test,
// Bonferroni-corrected per target, 15% bar): a regression names the
// queryid, a comparison inside the bars is clean, too little traffic is
// clean_low_traffic. Born queryids are listed and never judged. It reads
// only the review's expectations and evidence.
func Decide(r changereview.Review, w Windows, obs Observations) changereview.Receipt {
	ids := r.Expectations.WatchQueryIDs
	pooled, per := verify.DecideTargets(obs.Before, obs.After, ids,
		verify.Thresholds{RegressPct: RegressPct})
	rc := changereview.Receipt{
		WindowHours: int(math.Round(w.AfterTo.Sub(w.AfterFrom).Hours())),
		BeforeFrom:  w.BeforeFrom, BeforeTo: w.BeforeTo, AfterFrom: w.AfterFrom,
		AfterTo: w.AfterTo, Targets: targets(per),
		Tolerance: tolerance(r.Expectations.Predictions, per),
		Incidents: obs.Incidents, LockFindings: obs.LockFindings,
		BornQueryIDs: obs.Born, Confounders: obs.Confounders}
	switch {
	case len(obs.Incidents)+len(obs.LockFindings) > 0:
		rc.Verdict, rc.Reason = changereview.ReceiptRegressed, ReasonLock
		for _, id := range obs.Incidents {
			rc.ContradictedBy = append(rc.ContradictedBy, "incident:"+id)
		}
		for _, id := range obs.LockFindings {
			rc.ContradictedBy = append(rc.ContradictedBy, fmt.Sprintf("finding:%d", id))
		}
	case len(obs.Confounders) > 0:
		rc.Verdict, rc.Reason = changereview.ReceiptUnverifiable, ReasonConfounded
	case pooled.Verdict == verify.OutcomeRegressed:
		rc.Verdict, rc.Reason = changereview.ReceiptRegressed, ReasonLatency
		rc.ContradictedBy = regressedBy(rc.Targets)
	case pooled.Verdict == verify.OutcomeNeutral || pooled.Verdict == verify.OutcomeImproved:
		rc.Verdict, rc.Reason = changereview.ReceiptClean, ReasonNoChange
	default:
		rc.Verdict, rc.Reason = changereview.ReceiptCleanLowTraffic, ReasonLowTraffic
	}
	rc.EvidenceIDs = cited(r, rc)
	return rc
}

// targets is the per-target comparisons as receipt targets.
func targets(per []verify.TargetComparison) []changereview.ReceiptTarget {
	out := make([]changereview.ReceiptTarget, 0, len(per))
	for _, tc := range per {
		b, a := ms(tc.Before.AverageLatency), ms(tc.After.AverageLatency)
		t := changereview.ReceiptTarget{QueryID: tc.QueryID, Verdict: tc.Verdict,
			BeforeMeanMs: b, AfterMeanMs: a, BeforeCalls: int64(tc.Before.Samples),
			AfterCalls: int64(tc.After.Samples)}
		if b > 0 && tc.After.Samples > 0 {
			t.ChangePct = (a - b) * 100 / b
		}
		if p := pValue(tc.T, tc.DF); tc.DF > 0 && !math.IsNaN(p) {
			t.PValue = p
		}
		out = append(out, t)
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// regressedBy names the regressed targets, or, when only their pool
// regressed, the measured target that rose most: no penalty without a
// named queryid.
func regressedBy(ts []changereview.ReceiptTarget) []string {
	var out []string
	worst, worstPct := int64(0), math.Inf(-1)
	for _, t := range ts {
		if t.Verdict == verify.OutcomeRegressed {
			out = append(out, fmt.Sprintf("queryid:%d", t.QueryID))
		}
		if t.BeforeCalls > 0 && t.AfterCalls > 0 && t.ChangePct > worstPct {
			worst, worstPct = t.QueryID, t.ChangePct
		}
	}
	if len(out) == 0 && !math.IsInf(worstPct, -1) {
		out = []string{fmt.Sprintf("queryid:%d", worst)}
	}
	return out
}

// tolerance combines each predicted target's tolerance verdict: missed if
// any missed, met if all met, unmeasured if none was measured, partial
// otherwise; no_prediction when no target was predicted.
func tolerance(preds []verify.Prediction, per []verify.TargetComparison) string {
	byID := map[int64]verify.Prediction{}
	for _, p := range preds {
		for _, id := range p.TargetQueryIDs {
			byID[id] = p
		}
	}
	counts := map[string]int{}
	n := 0
	for _, tc := range per {
		p, ok := byID[tc.QueryID]
		if !ok || !p.Predicts() {
			continue
		}
		var observed *float64
		if tc.Verdict != verify.OutcomeInsufficient {
			pct := tc.DeltaPct
			observed = &pct
		}
		counts[verify.ToleranceVerdict(p, tc.Verdict, observed)]++
		n++
	}
	switch {
	case n == 0:
		return verify.ToleranceNoPrediction
	case counts[verify.ToleranceMissed] > 0:
		return verify.ToleranceMissed
	case counts[verify.ToleranceMet] == n:
		return verify.ToleranceMet
	case counts[verify.ToleranceUnmeasured] == n:
		return verify.ToleranceUnmeasured
	}
	return verify.TolerancePartial
}

// cited is the review evidence the receipt rests on: each target's
// queryid evidence and, for lock harm, the touched tables'.
func cited(r changereview.Review, rc changereview.Receipt) []string {
	held := map[string]bool{}
	for _, e := range r.Evidence {
		held[e.ID] = true
	}
	var out []string
	add := func(id string) {
		if held[id] {
			out = append(out, id)
			held[id] = false
		}
	}
	for _, t := range rc.Targets {
		add(fmt.Sprintf("queryid:%d", t.QueryID))
	}
	if rc.Reason == ReasonLock {
		for _, rel := range r.Expectations.Relations {
			add("table:" + rel)
		}
	}
	return out
}
