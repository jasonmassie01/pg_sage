package action

import "fmt"

// unprovenNote ends every reconciled outcome whose object cannot be tied to
// this proposal: Rollback needs an action ID and an object OID, so leaving
// the OID unset refuses the typed DROP of an object pg_sage cannot prove.
const unprovenNote = "; operator review required: the created object cannot be " +
	"proven, so typed rollback is refused"

// settleStranded decides the end state of a stranded proposal from what the
// catalog and the action ledger show. It returns the event type to emit, or
// "" (leaving p unchanged) when p is not stranded.
func settleStranded(p *MaintenanceProposal, o statisticsObservation) string {
	switch p.State {
	case "executing":
		return settleExecuting(p, o)
	case "rolling_back":
		return settleRollingBack(p, o)
	}
	return ""
}

// settleExecuting claims the object only when exactly one action record and
// an object on the bound table agree; nothing at all is a failed run, and
// every disagreement is uncertain with no object identity.
func settleExecuting(p *MaintenanceProposal, o statisticsObservation) string {
	lost := "the run ended without recording its outcome; "
	recorded := o.actions == 1 && o.outcome != "failed"
	p.State, p.Verification, p.StatisticsOID = "uncertain", "inconclusive", 0
	p.ActionLogID = o.actionID
	switch {
	case o.actions > 1:
		p.Detail = fmt.Sprintf("%s%d action records match this proposal%s", lost,
			o.actions, unprovenNote)
	case o.foreign:
		p.Detail = lost + "a statistics object with this proposal's name exists on " +
			"another table and is not claimed" + unprovenNote
	case recorded && o.objectOID != 0:
		p.StatisticsOID = o.objectOID
		p.Detail = lost + "the recorded action and the object on the bound table " +
			"agree; the build is not re-verified and typed rollback is available"
	case o.objectOID == 0 && o.actions == 0:
		p.State, p.Verification = "failed", "failed"
		p.Detail = lost + "the executor recorded no action and no statistics object exists"
	case o.objectOID == 0 && !recorded:
		p.State, p.Verification = "failed", "failed"
		p.Detail = lost + "the executor recorded a failed run and no statistics " +
			"object exists"
	case o.objectOID == 0:
		p.Detail = lost + "an action is recorded but the statistics object is absent" +
			unprovenNote
	case o.actions == 0:
		p.Detail = lost + "the statistics object exists but no action record matches" +
			unprovenNote
	default:
		p.Detail = lost + "the executor recorded a failure but the statistics object " +
			"exists" + unprovenNote
	}
	return "action_failed"
}

// settleRollingBack keeps the proposal's identity: the created object is
// present (retry the rollback) or gone (rolled back). An object that now
// holds the name with another OID is not the created one and is not touched.
func settleRollingBack(p *MaintenanceProposal, o statisticsObservation) string {
	lost := "the rollback ended without recording its outcome; "
	if o.objectOID != 0 && o.objectOID == p.StatisticsOID {
		p.State, p.Verification = "rollback_failed", "failed"
		p.Detail = lost + "the created object still exists; retry the rollback"
		return "action_failed"
	}
	p.State, p.Verification = "rolled_back", "object_removed"
	p.Detail = lost + "the created object is absent; prior ANALYZE statistics " +
		"cannot be restored"
	if o.objectOID != 0 || o.foreign {
		p.Detail += "; a different object now holds its name and was not touched"
	}
	return "action_executed"
}
