package verify

import "fmt"

// MaintenanceGainShare is the share of the baseline VACUUM/ANALYZE must
// remove to be an improvement.
const MaintenanceGainShare = 0.5

// MaintenanceJudgement is the verdict of a VACUUM or ANALYZE from its
// metric before and after.
type MaintenanceJudgement struct {
	Verdict     string
	Reason      string
	Metric      string
	Before      float64
	After       float64
	ObservedPct *float64
}

// JudgeMaintenanceMetric decides VACUUM/ANALYZE from the metric before
// and after: improved when at least half of it went, neutral when less did
// (an old snapshot holding dead tuples back), insufficient when there was
// nothing to remove, unverifiable when either side could not be read
// (negative) or the class is no maintenance. pg_sage's own maintenance and
// a maintenance run outside pg_sage (shadow mode) are judged by this bar.
func JudgeMaintenanceMetric(class, metric string, before, after float64) MaintenanceJudgement {
	j := MaintenanceJudgement{Before: before, After: after, Metric: metric}
	switch {
	case class != ClassVacuum && class != ClassAnalyze:
		j.Verdict, j.Reason = OutcomeUnverifiable, "no maintenance metric for "+class
		return j
	case before < 0 || after < 0:
		j.Verdict, j.Reason = OutcomeUnverifiable, metric+" could not be read"
		return j
	case before < 1:
		j.Verdict, j.Reason = OutcomeInsufficient, "nothing to remove before the "+
			"action ("+metric+" was 0)"
		return j
	}
	change := (after - before) * 100 / before
	j.ObservedPct = &change
	if after <= before*(1-MaintenanceGainShare) {
		j.Verdict = OutcomeImproved
		j.Reason = fmt.Sprintf("%s fell from %.0f to %.0f", metric, before, after)
		return j
	}
	j.Verdict = OutcomeNeutral
	j.Reason = fmt.Sprintf("%s barely moved (%.0f -> %.0f)", metric, before, after)
	return j
}
