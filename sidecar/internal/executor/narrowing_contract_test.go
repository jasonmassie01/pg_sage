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

// The freeze and kill contracts are narrowing (§6.2.4); unfreeze widens
// and is not. All three are agent_access and supported everywhere (§6.3).
func TestGuardFreezeKillUnfreezeContracts(t *testing.T) {
	cases := map[string]struct {
		risk      string
		rollback  string
		narrowing bool
	}{
		ActionTypeGuardFreeze:   {"safe", "reversible", true},
		ActionTypeGuardKill:     {"safe", "reversible", true},
		ActionTypeGuardUnfreeze: {"moderate", "reversible", false},
	}
	for at, want := range cases {
		c, ok := ContractForActionType(at)
		if !ok {
			t.Fatalf("%s has no contract", at)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", at, err)
		}
		if c.BaseRiskTier != want.risk || c.RollbackClass != want.rollback ||
			c.Narrowing != want.narrowing {
			t.Fatalf("%s = %+v, want %+v", at, c, want)
		}
		if len(c.ProviderSupport) != len(guardProviders()) {
			t.Fatalf("%s providers = %v", at, c.ProviderSupport)
		}
		if got := changeClassForActionType(at); got != "agent_access" {
			t.Fatalf("%s change class = %q", at, got)
		}
		pc, ok := PolicyContractFor(at)
		if !ok || pc.Narrowing != want.narrowing || pc.ActionType != at {
			t.Fatalf("%s policy contract = %+v", at, pc)
		}
	}
}
