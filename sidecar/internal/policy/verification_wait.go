package policy

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// One self-initiated change per object (dogfood lifeos, 2026-10-04): a
// second change to a GUC or a table while the first change's verification
// is still in flight makes both verdicts meaningless (work_mem 9MB then
// 10MB 39 minutes later; a second index on public.memories ten minutes
// after the first). The gate parks such a change until the verification
// concludes, or until its hard deadline passes without a verdict, so it
// never parks forever. Emergencies and rollbacks of pg_sage's own changes
// are exempt; a person's approval overrides the wait and is recorded.

// ReasonAwaitingVerification parks a self-initiated change to an object
// whose previous change is still being verified; Decision.Detail names the
// action and until when.
const ReasonAwaitingVerification Reason = "awaiting_verification"

// PendingVerification is one change in flight on the request's object:
// an executed action whose verification has not concluded, or (ActionID
// 0) a change authorized by DecisionID that has not run yet.
type PendingVerification struct {
	ActionID   int64
	DecisionID int64
	// Object is the shared identity: "guc:<name>" or "table:<schema.table>".
	Object string
	// Until is when the verdict is next due: the end of the first window,
	// or the hard deadline once that has passed.
	Until time.Time
	// HardDeadline is when the wait is released without a verdict.
	HardDeadline time.Time
}

// Expired reports a wait whose hard deadline has passed (inclusive).
func (p PendingVerification) Expired(now time.Time) bool {
	return !p.HardDeadline.IsZero() && !now.Before(p.HardDeadline)
}

// VerificationTracker lists the changes in flight on a request's objects.
// An error means the in-flight state could not be read.
type VerificationTracker interface {
	PendingVerifications(context.Context, ActionRequest) ([]PendingVerification, error)
}

// VerificationWait is what the gate saw: the waits that park the request
// (or that an operator approval overrode) and those released at their hard
// deadline.
type VerificationWait struct {
	Pending    []PendingVerification
	Released   []PendingVerification
	Overridden bool
}

// awaitVerification applies the wait to the gate's verdict. It only
// restricts: an execute becomes a park; an approval stays queued with the
// wait in its detail; an operator approval executes and records the
// override. Other verdicts, exempt and read-only requests pass through.
func (gate *authorizationGate) awaitVerification(
	ctx context.Context, req ActionRequest, base Decision,
) Decision {
	if gate.config.Verification == nil || !waitApplies(req, base, gate.now()) {
		return base
	}
	pending, err := gate.config.Verification.PendingVerifications(ctx, req)
	if err != nil {
		why := "read in-flight verifications: " + err.Error()
		if req.OperatorApproved {
			base.Detail = joinDetail(base.Detail, why)
			return base
		}
		return decisionForRequest(req, blocked(ReasonPolicyUnavailable, why))
	}
	wait := splitWaits(pending, gate.now())
	switch {
	case len(wait.Pending) == 0 && len(wait.Released) == 0:
		return base
	case len(wait.Pending) == 0:
		base.Detail = joinDetail(base.Detail, ReleaseDetail(wait.Released))
	case req.OperatorApproved:
		wait.Overridden = true
		base.Detail = joinDetail(base.Detail, OverrideDetail(wait.Pending))
	case base.Verdict == VerdictExecute:
		park := decisionForRequest(req, parked(ReasonAwaitingVerification,
			WaitDetail(wait.Pending)))
		park.BudgetKind, park.RowsRewritten = base.BudgetKind, base.RowsRewritten
		park.VerificationWait = wait
		return park
	default:
		base.Detail = joinDetail(base.Detail, WaitDetail(wait.Pending))
	}
	base.VerificationWait = wait
	return base
}

// waitApplies reports a mutation whose execute or approval verdict the
// wait restricts. Owner-declared work (retention under its contract) is
// not pg_sage's own initiative; emergency mitigations, reverts and
// rollbacks of pg_sage's own changes (BudgetBypassFor) must not wait.
func waitApplies(req ActionRequest, base Decision, now time.Time) bool {
	if req.Contract == nil || requestIsReadOnly(req) || req.OwnerDeclared {
		return false
	}
	if base.Verdict != VerdictExecute && base.Verdict != VerdictQueueApproval {
		return false
	}
	return BudgetBypassFor(req, now) == ""
}

func splitWaits(pending []PendingVerification, now time.Time) *VerificationWait {
	wait := &VerificationWait{}
	for _, p := range pending {
		if p.Expired(now) {
			wait.Released = append(wait.Released, p)
		} else {
			wait.Pending = append(wait.Pending, p)
		}
	}
	return wait
}

// WaitDetail names each change in flight, until when and on which object:
// "awaiting verification of action 6407 (until 2026-10-04T18:10:00Z) on
// guc:work_mem".
func WaitDetail(pending []PendingVerification) string {
	parts := make([]string, 0, len(pending))
	for i, p := range pending {
		lead := "awaiting "
		if i > 0 {
			lead = "and "
		}
		parts = append(parts, fmt.Sprintf("%s%s (until %s) on %s", lead, pendingName(p),
			formatWaitTime(p.Until), p.Object))
	}
	return strings.Join(parts, "; ")
}

// OverrideDetail is the operator override as the approval card and the
// decision log show it.
func OverrideDetail(pending []PendingVerification) string {
	if len(pending) == 0 {
		return ""
	}
	names := make([]string, 0, len(pending))
	for _, p := range pending {
		names = append(names, pendingName(p))
	}
	return "overrides pending " + strings.Join(names, ", ")
}

// ReleaseDetail records waits released at their hard deadline.
func ReleaseDetail(released []PendingVerification) string {
	parts := make([]string, 0, len(released))
	for _, p := range released {
		parts = append(parts, fmt.Sprintf("%s passed its hard deadline %s without a "+
			"verdict: wait released", pendingName(p), formatWaitTime(p.HardDeadline)))
	}
	return strings.Join(parts, "; ")
}

// pendingName is "verification of action N", or for a change not yet run
// "the change authorized by decision N".
func pendingName(p PendingVerification) string {
	if p.ActionID > 0 {
		return fmt.Sprintf("verification of action %d", p.ActionID)
	}
	return fmt.Sprintf("the change authorized by decision %d", p.DecisionID)
}

func formatWaitTime(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return at.UTC().Format(time.RFC3339)
}

func joinDetail(detail, more string) string {
	switch {
	case more == "":
		return detail
	case detail == "":
		return more
	}
	return detail + "; " + more
}
