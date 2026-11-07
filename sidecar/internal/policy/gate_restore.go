package policy

import (
	"context"
	"strings"
)

// ActionRestoreDroppedIndex re-creates, CONCURRENTLY, the recorded
// definition of an index pg_sage dropped itself (the soft drop).
const ActionRestoreDroppedIndex = "restore_dropped_index"

// IsDropRestore reports the soft-drop restore of pg_sage's own index drop:
// a rollback request under the restore contract. The executor sends it only
// after the SQL matched the dropped index's recorded definition.
func IsDropRestore(req ActionRequest) bool {
	return req.Rollback && req.Contract != nil &&
		req.Contract.ActionType == ActionRestoreDroppedIndex
}

// dropRestoreDecision authorizes the soft-drop restore (executor-01, owner
// decision 2026-11-05). The restore was a condition of the autonomy the
// drop ran under and only re-creates what existed, so the trust
// downgrades (observation, manual or approval mode, tier3_safe off, an
// unsatisfied ramp) do not strand it: observeOnly and tierDecision are
// skipped. The hard stops have already bound (evaluate); the policy
// document (change classes, approval classes, refusal set), degraded SQL
// validation and the lock ceiling still bind.
func (gate *authorizationGate) dropRestoreDecision(
	ctx context.Context, runtime RuntimeState, req ActionRequest,
) Decision {
	switch runtime.TrustLevel {
	case TrustObservation, TrustAdvisory, TrustAutonomous:
	default:
		return blocked(ReasonUnknownTrustLevel, runtime.TrustLevel)
	}
	doc, decision, stop, note := gate.documentDecision(ctx, req)
	if stop {
		return note.stamp(decision)
	}
	if decision, refused := refusalDecision(doc, req); refused {
		return note.stamp(decision)
	}
	if runtime.SQLValidationDegraded {
		return note.stamp(decisionForRequest(req, degradedValidationDecision()))
	}
	restore := gate.decision(req, VerdictExecute, ReasonDropRestore)
	restore.Detail = gate.dropRestoreDetail(runtime, req, note.bypass)
	return note.stamp(withDocumentBounds(doc, restore))
}

// dropRestoreDetail names the restore and each trust downgrade it passed,
// so the ledger shows why a restore ran under lowered trust.
func (gate *authorizationGate) dropRestoreDetail(
	runtime RuntimeState, req ActionRequest, bypass string,
) string {
	var passed []string
	if runtime.TrustLevel == TrustObservation {
		passed = append(passed, "trust observation")
	}
	if runtime.ExecutionMode == ExecutionManual || runtime.ExecutionMode == ExecutionApproval {
		passed = append(passed, "execution_mode "+runtime.ExecutionMode)
	}
	if !runtime.Tier3Safe {
		passed = append(passed, "tier3_safe off")
	}
	ramp := rampAge(runtime.SafeRampAge, SpecSafeRampAge, req.Contract.RollbackClass)
	if !rampSatisfied(runtime, ramp, gate.now(), false) {
		passed = append(passed, "trust ramp not satisfied")
	}
	detail := "restore of an index pg_sage dropped"
	if bypass != "" {
		detail = "budget bypass: " + bypass
	}
	if len(passed) > 0 {
		detail += "; trust downgrade bypassed: " + strings.Join(passed, ", ")
	}
	return detail
}
