package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-07 review: a custodian escalation is recorded as a blocked
// decision with reasonCustodianBackoff, so GET /api/v1/decisions must
// accept it as a reason filter; policy.LedgerReasons lists it by value.
func TestCustodianBackoffIsALedgerReason(t *testing.T) {
	if !policy.IsLedgerReason(reasonCustodianBackoff) {
		t.Fatalf("policy.IsLedgerReason(%q) = false: the decisions route would refuse "+
			"to filter the custodian escalations it lists", reasonCustodianBackoff)
	}
}
