package runbook_test

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Every action type a runbook may propose is a typed action that exists:
// it has an executor action contract (AI-SRE-SPEC §7.1/§7.2). The runbook
// package cannot import the executor (the executor depends on the
// investigator), so the binding is checked here.
func TestActionTypes_EachHasAnExecutorContract(t *testing.T) {
	for _, a := range runbook.ActionTypes() {
		c, ok := executor.ContractForActionType(a)
		if !ok {
			t.Errorf("runbook action %s has no executor contract", a)
			continue
		}
		if err := c.Validate(); err != nil || c.ActionType != a {
			t.Errorf("runbook action %s: contract %q invalid: %v", a, c.ActionType, err)
		}
	}
}
