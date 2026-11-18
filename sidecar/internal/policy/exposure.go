package policy

import (
	"context"
	"fmt"
	"regexp"
)

// Exposure remediation (fleetpol-02b). The policy document's exposure mode
// decides whether pg_sage may act on what the exposure detectors find:
// shadow (the default) records the would-be action only; enforce hands
// revoke_public_grant and enable_row_level_security to the usual tier,
// trust and approval rules.
const (
	ExposureModeShadow  = "shadow"
	ExposureModeEnforce = "enforce"

	// ActionRevokePublicGrant revokes a table's PUBLIC grants (moderate,
	// reversible: the stored re-GRANT).
	ActionRevokePublicGrant = "revoke_public_grant"
	// ActionEnableRowLevelSecurity enables RLS on a table (high, always
	// approval: it fails closed for every reader without a policy).
	ActionEnableRowLevelSecurity = "enable_row_level_security"

	// ChangeExposure is the change class of both exposure contracts.
	ChangeExposure ChangeClass = "exposure"

	// ReasonExposureShadow: the document's exposure mode is shadow.
	ReasonExposureShadow Reason = "exposure_shadow"
)

type exposureWire struct {
	Mode string `json:"mode"`
}

// ExposureEnforced reports whether the document's exposure mode is enforce.
// An absent mode is shadow.
func (d Document) ExposureEnforced() bool { return d.ExposureMode == ExposureModeEnforce }

func validateExposureMode(mode string) error {
	switch mode {
	case "", ExposureModeShadow, ExposureModeEnforce:
		return nil
	default:
		return fmt.Errorf("exposure.mode %q must be %s or %s", mode, ExposureModeShadow,
			ExposureModeEnforce)
	}
}

func exposureFromWire(wire *exposureWire) string {
	if wire == nil {
		return ""
	}
	return wire.Mode
}

// exposureWidens: enforce where the base was not.
func exposureWidens(base, next Document) bool {
	return next.ExposureEnforced() && !base.ExposureEnforced()
}

// IsExposureRemediation reports a request under one of the two exposure
// contracts.
func IsExposureRemediation(req ActionRequest) bool {
	if req.Contract == nil {
		return false
	}
	switch req.Contract.ActionType {
	case ActionRevokePublicGrant, ActionEnableRowLevelSecurity:
		return true
	default:
		return false
	}
}

// exposureInverseSQL is a re-grant to PUBLIC or an RLS disable: the
// rollback forms of the two contracts, never a forward action.
var exposureInverseSQL = regexp.MustCompile(`(?i)^\s*GRANT\b|\bDISABLE\s+ROW\s+LEVEL\s+` +
	`SECURITY\b`)

// exposureRollback is the revert of pg_sage's own exposure change: it
// restores the prior grant or RLS state, so the refusal set's
// grant_expansion and rls_change do not hold it.
func exposureRollback(req ActionRequest) bool {
	return req.Rollback && IsExposureRemediation(req)
}

// exposureDecision binds every forward exposure request, approved ones
// included: an inverse statement is refused, and unless the document's
// mode is enforce the request is observe-only. A rollback is decided
// before this (rollbackDecision) and is never held by the mode.
func (gate *authorizationGate) exposureDecision(
	ctx context.Context, req ActionRequest,
) (Decision, bool) {
	if !IsExposureRemediation(req) || req.Rollback {
		return Decision{}, false
	}
	if exposureInverseSQL.MatchString(req.SQL) {
		decision := gate.decision(req, VerdictBlocked, ReasonRefusedByPolicy)
		decision.Detail = "a re-grant or RLS disable runs only as the rollback of a " +
			"recorded exposure action"
		return decision, true
	}
	doc, err := gate.policy(ctx, req)
	if err != nil || ValidateDocument(doc) != nil {
		return blocked(ReasonPolicyUnavailable, errorDetail(err)), true
	}
	if doc.ExposureEnforced() {
		return Decision{}, false
	}
	decision := gate.decision(req, VerdictObserveOnly, ReasonExposureShadow)
	decision.Detail = "exposure.mode is shadow: the finding records the would-be action; " +
		"set exposure.mode to enforce to propose it"
	return decision, true
}
