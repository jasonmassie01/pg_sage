package executor

import "testing"

// The executor contract's Narrowing flag reaches the gate's contract
// (AGENTDB-SPEC §6.2.4); without it a narrowing action would stop at the
// emergency stop like any other.
func TestPolicyContractCarriesNarrowing(t *testing.T) {
	c := ActionContract{ActionType: "guard_freeze", BaseRiskTier: "safe",
		RollbackClass: "reversible", PostChecks: []string{"flag set"}, Narrowing: true}
	got := policyContract(c)
	if !got.Narrowing {
		t.Fatal("policyContract dropped Narrowing")
	}
	c.Narrowing = false
	if policyContract(c).Narrowing {
		t.Fatal("policyContract set Narrowing on a non-narrowing contract")
	}
}

// No existing contract is narrowing: only the agent governance contracts
// that take access away may set it.
func TestExistingContractsAreNotNarrowing(t *testing.T) {
	for _, at := range []string{"create_index_concurrently", "vacuum", "analyze",
		"cancel_backend", "terminate_backend", ActionTypeGuardRoleEnsure,
		ActionTypeGuardRoleRetire} {
		c, ok := ContractForActionType(at)
		if !ok {
			continue
		}
		if c.Narrowing {
			t.Fatalf("%s is narrowing", at)
		}
	}
}
