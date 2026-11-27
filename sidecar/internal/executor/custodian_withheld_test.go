package executor

import (
	"errors"
	"strings"
	"testing"
)

// vacuum-09 error propagation: a custodian route withheld by the
// maintenance precheck must keep the precheck's cause, so the caller can
// tell a missing grant (fixable by the operator) from any other refusal,
// as the operator path already can (ManualExecuteStatus). Pure function:
// no concurrency or database case applies.

func TestCustodianWithheldKeepsThePrivilegeCause(t *testing.T) {
	withheld := &WithheldError{Decision: ActionPolicyDecision{
		BlockedReason: reasonCapabilityMissing}}
	cause := &causedWithheldError{WithheldError: withheld, cause: ErrMaintenancePrivilegeMissing}

	err := custodianWithheld(cause, "after admission")

	if !errors.Is(err, ErrCustodianProposalWithheld) ||
		!errors.Is(err, ErrMaintenancePrivilegeMissing) {
		t.Fatalf("err = %v, want both the custodian withhold and the missing privilege", err)
	}
	var asWithheld *WithheldError
	if errors.As(err, &asWithheld) {
		t.Fatalf("err = %v unwraps to a *WithheldError; custodian routes must stay distinct", err)
	}
	if !strings.Contains(err.Error(), reasonCapabilityMissing) ||
		!strings.Contains(err.Error(), ErrMaintenancePrivilegeMissing.Error()) {
		t.Fatalf("message = %q, want the reason and the cause", err.Error())
	}
}

func TestCustodianWithheldWithoutCauseIsUnchanged(t *testing.T) {
	withheld := &WithheldError{Decision: ActionPolicyDecision{BlockedReason: "blast_radius"}}

	err := custodianWithheld(withheld, "after lease")

	if err.Error() != "custodian proposal withheld by policy: blast_radius" ||
		!errors.Is(err, ErrCustodianProposalWithheld) ||
		errors.Is(err, ErrMaintenancePrivilegeMissing) {
		t.Fatalf("err = %v, want the plain custodian withhold", err)
	}
}

func TestCustodianWithheldPassesOtherErrorsThrough(t *testing.T) {
	other := errors.New("connection refused")
	if err := custodianWithheld(other, "after lease"); err != other {
		t.Fatalf("err = %v, want the original error", err)
	}
	if err := custodianWithheld(nil, "after lease"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
