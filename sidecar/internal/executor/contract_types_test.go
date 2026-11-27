package executor

import "testing"

// ContractActionTypes must list every ContractForActionType case exactly
// once, so readiness can derive provider families from the contracts.
func TestContractActionTypesMatchesContracts(t *testing.T) {
	listed := map[string]bool{}
	for _, actionType := range ContractActionTypes() {
		if listed[actionType] {
			t.Errorf("%s listed twice", actionType)
		}
		listed[actionType] = true
		if c, ok := ContractForActionType(actionType); !ok || c.ActionType != actionType {
			t.Errorf("%s has no contract (got %q)", actionType, c.ActionType)
		}
	}
	for _, actionType := range contractActionTypes() {
		if !listed[actionType] {
			t.Errorf("contract %s missing from ContractActionTypes", actionType)
		}
	}
	for _, actionType := range []string{ActionTypeRestoreDroppedIndex, ActionTypeReplaceIndex,
		ActionTypeGuardRoleEnsure, ActionTypeGuardRevoke} {
		if !listed[actionType] {
			t.Errorf("constant-named contract %s missing", actionType)
		}
	}
	got := ContractActionTypes()
	got[0] = "mutated"
	if ContractActionTypes()[0] == "mutated" {
		t.Fatal("ContractActionTypes exposes its backing slice")
	}
}
