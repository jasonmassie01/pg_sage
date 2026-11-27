package tuner

import "time"

// Hint revalidation decisions (tuning-01). Revalidation never retires a
// hint for working or for being old. It marks a hint broken when a
// referenced index is gone, dormant while its query does not run, and due
// for re-verification past HintRetirementDays; at re-verification the
// hinted EXPLAIN (tuning-05) retires a hint that no longer changes the plan.

type hintAction int

const (
	// hintActionKeep: the query ran since the last pass; the hint is active.
	hintActionKeep hintAction = iota
	// hintActionDormant: no new calls, or the queryid is absent. The hint
	// stays installed and returns to active once calls advance again.
	hintActionDormant
	// hintActionReset: pg_stat_statements was reset (or the counter went
	// down). The checkpoint is re-baselined and the status is unchanged.
	hintActionReset
	// hintActionBroken: a referenced index is missing from the catalog.
	hintActionBroken
	// hintActionError: the statistics could not be read; nothing changes.
	hintActionError
	// hintActionRetire: due for re-verification, the hinted EXPLAIN plans
	// the statement as without the hint (no_effect, tuning-05).
	hintActionRetire
)

// pgssObservation is one revalidation read of pg_stat_statements for a
// queryid in the current database, summed over its rows (one per user and
// toplevel flag), plus pg_stat_statements_info.stats_reset.
type pgssObservation struct {
	Calls   int64
	MeanMs  float64
	Found   bool
	ResetAt *time.Time
}

type hintDecision struct {
	action       hintAction
	reason       string
	currentCalls int64
	// checkpoint is the new calls_at_last_check; nil leaves it unchanged.
	checkpoint *int64
	// resetAt is the stats_reset to record; nil leaves it unchanged.
	resetAt *time.Time
	// reverifyDue stamps reverify_due (reason age_reverify) when unset.
	reverifyDue bool
}

// nextStatus is the sage.query_hints status the decision leaves h in.
func (d hintDecision) nextStatus(h hintRow) string {
	switch d.action {
	case hintActionKeep:
		return "active"
	case hintActionDormant:
		return "dormant"
	case hintActionBroken:
		return "broken"
	case hintActionRetire:
		return "retired"
	default: // reset and error keep the current status
		return h.Status
	}
}

// decideFromObservation decides a hint's fate from one statistics read. The
// referenced-object check (broken) runs before it, in decideHintFate.
func decideFromObservation(
	h hintRow, obs pgssObservation, retentionCutoff time.Duration, now time.Time,
) hintDecision {
	d := hintDecision{
		currentCalls: obs.Calls,
		resetAt:      obs.ResetAt,
		reverifyDue: retentionCutoff > 0 && h.ReverifyDue == nil &&
			now.Sub(h.CreatedAt) > retentionCutoff,
	}
	calls := obs.Calls
	switch {
	case statsWereReset(h, obs):
		d.action, d.reason, d.checkpoint = hintActionReset, "stats_reset", &calls
	case !obs.Found:
		d.action, d.reason = hintActionDormant, "queryid absent from pg_stat_statements"
	case h.CallsAtLastCheck != nil && calls == *h.CallsAtLastCheck:
		d.action, d.reason = hintActionDormant, "no new executions since last revalidation"
		d.checkpoint = &calls
	default:
		d.action, d.checkpoint = hintActionKeep, &calls
		if h.Status == "dormant" {
			d.reason = "executions resumed"
		}
	}
	return d
}

// statsWereReset reports a pg_stat_statements reset, crash or eviction
// since the last pass: stats_reset moved forward, or the summed calls went
// down.
func statsWereReset(h hintRow, obs pgssObservation) bool {
	if obs.ResetAt != nil && h.PgssResetAt != nil && obs.ResetAt.After(*h.PgssResetAt) {
		return true
	}
	return obs.Found && h.CallsAtLastCheck != nil && obs.Calls < *h.CallsAtLastCheck
}
