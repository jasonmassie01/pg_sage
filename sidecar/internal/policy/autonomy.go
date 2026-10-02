package policy

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Earned-autonomy reasons (Sage SRE M7, AI-SRE-SPEC §7.3).
const (
	// ReasonAutonomyLevel withholds a self-initiated family action below L2:
	// pg_sage shows the proposal as a manual script.
	ReasonAutonomyLevel Reason = "autonomy_level"
	// ReasonAutonomyHandoff hands the action to a human as a one-click
	// approval (L2, or L3 when an L3 condition does not hold).
	ReasonAutonomyHandoff Reason = "autonomy_handoff"
	// ReasonAutonomyL3 executes a reversible, single-object action inside
	// the maintenance window; a human is notified.
	ReasonAutonomyL3 Reason = "autonomy_l3"
	// ReasonAutonomyDowngraded withholds the action while a downgrade
	// signal holds (error budget, failover, stale evidence, concurrency).
	ReasonAutonomyDowngraded Reason = "autonomy_downgraded"
	// ReasonAutonomyUnavailable fails closed when the ledger cannot answer.
	ReasonAutonomyUnavailable Reason = "autonomy_unavailable"
)

// AutonomyLimit is the ledger's answer for one request.
type AutonomyLimit struct {
	// Level is the effective level (0..3) the ledger allows now.
	Level int
	// Granted is the human-approved level of the family x class pair.
	Granted int
	// Downgraded reports an active downgrade signal; Reasons names them.
	Downgraded bool
	Reasons    []string
	// Class is the action class the ledger resolved.
	Class string
}

// AutonomyLimiter answers the earned-autonomy level for a request.
type AutonomyLimiter interface {
	Limit(context.Context, ActionRequest) (AutonomyLimit, error)
}

const (
	autonomyHandoffLevel = 2
	autonomyExecuteLevel = 3
	// rollbackMitigationOnlyClass is the M5 mitigation-only class (an
	// approved backend cancel): it can be handed off, never auto-executed.
	rollbackMitigationOnlyClass RollbackClass = "mitigation_only"
)

// governedByAutonomy reports whether the ledger restricts req: a
// self-initiated mutation that remediates an incident family.
func (gate *authorizationGate) governedByAutonomy(req ActionRequest) bool {
	return gate.config.Autonomy != nil && strings.TrimSpace(req.IncidentFamily) != "" &&
		!req.OperatorApproved && req.Contract != nil &&
		req.Contract.RiskTier != RiskReadOnly
}

// restrictAutonomy applies the ledger to an execute or approval verdict.
// It can only restrict: blocked, parked and observe-only verdicts pass
// through, and no level turns a non-execute verdict into execute.
func (gate *authorizationGate) restrictAutonomy(
	ctx context.Context, doc Document, runtime RuntimeState, req ActionRequest,
	base Decision,
) Decision {
	if !gate.governedByAutonomy(req) ||
		(base.Verdict != VerdictExecute && base.Verdict != VerdictQueueApproval) {
		return base
	}
	limit, err := gate.config.Autonomy.Limit(ctx, req)
	if err != nil {
		return decisionForRequest(req, blocked(ReasonAutonomyUnavailable, err.Error()))
	}
	level := effectiveAutonomyLevel(limit, req.Contract.RollbackClass)
	note := autonomyNote(level, limit, base)
	switch {
	case level < autonomyHandoffLevel && limit.Downgraded:
		return restricted(req, VerdictObserveOnly, ReasonAutonomyDowngraded, note)
	case level < autonomyHandoffLevel:
		return restricted(req, VerdictObserveOnly, ReasonAutonomyLevel, note)
	case level == autonomyHandoffLevel || base.Verdict == VerdictQueueApproval:
		return restricted(req, VerdictQueueApproval, ReasonAutonomyHandoff, note)
	}
	if why := l3Blocker(doc, runtime, req, gate.now()); why != "" {
		return restricted(req, VerdictQueueApproval, ReasonAutonomyHandoff, why+"; "+note)
	}
	base.Reason, base.OffWindowOK = ReasonAutonomyL3, false
	base.Detail = note
	return base
}

// effectiveAutonomyLevel clamps the ledger's level to L0..L3 (L4 is
// reserved), to L1 while downgraded, and to the contract's own
// reversibility, so a wrong ledger answer cannot exceed the contract.
func effectiveAutonomyLevel(limit AutonomyLimit, rollback RollbackClass) int {
	level := min(max(limit.Level, 0), autonomyExecuteLevel)
	if limit.Downgraded {
		level = min(level, 1)
	}
	return min(level, reversibilityCap(rollback))
}

// reversibilityCap is the highest level a rollback class allows:
// irreversible and unknown classes never exceed L1.
func reversibilityCap(class RollbackClass) int {
	switch class {
	case RollbackReversible, RollbackNoRollbackNeeded:
		return autonomyExecuteLevel
	case rollbackMitigationOnlyClass:
		return autonomyHandoffLevel
	}
	return 1
}

// l3Blocker names the L3 condition a request misses: exactly one target
// object, and both the standing-policy window and any configured window
// open. A deadline override does not stand in for the window at L3.
func l3Blocker(doc Document, runtime RuntimeState, req ActionRequest, now time.Time) string {
	if len(req.TargetObjs) != 1 {
		return "unbounded: L3 acts on exactly one object"
	}
	if !inAnyWindow(doc.MaintenanceWindows, now) ||
		(runtime.WindowConfigured && !runtime.InConfiguredWindow) {
		return "outside_window: L3 acts only inside the maintenance window"
	}
	return ""
}

func autonomyNote(level int, limit AutonomyLimit, base Decision) string {
	note := fmt.Sprintf("autonomy L%d (granted L%d, class %s); restricted %s",
		level, limit.Granted, limit.Class, base.Reason)
	if base.Detail != "" {
		note += " (" + base.Detail + ")"
	}
	if limit.Downgraded {
		note += "; downgraded: " + strings.Join(limit.Reasons, ",")
	}
	return note
}

func restricted(req ActionRequest, verdict Verdict, reason Reason, detail string) Decision {
	decision := decisionForRequest(req, blockedAs(verdict, reason))
	decision.Detail = detail
	return decision
}
